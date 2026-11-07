package futu

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
	qotgetbasicqot "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getbasicqot"
	qotgetsecuritysnapshot "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getsecuritysnapshot"
	qotsub "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_sub"
	qotupdatebasicqot "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_updatebasicqot"
)

// ErrInvalidCode is an instrument code that is not OpenD's market-prefixed
// form, HK.00700, on a market this client knows.
var ErrInvalidCode = errors.New("opend instrument code invalid")

// ErrQuotaExhausted is a subscribe OpenD refused with no subscription quota
// remaining. The quota is OpenD-wide: another client may have spent it. The
// fact comes from OpenD's own quota read after the refusal, never from its
// free text.
var ErrQuotaExhausted = errors.New("opend subscription quota exhausted")

// ErrSubscriptionHeld is a release before the documented minimum hold.
var ErrSubscriptionHeld = errors.New("opend subscription inside its minimum hold")

// SubscriptionHeldError is one refused release, with how long is left.
type SubscriptionHeldError struct {
	Code       string
	RetryAfter time.Duration
}

// Error names the line and the wait.
func (e *SubscriptionHeldError) Error() string {
	return fmt.Sprintf("%v: %s for another %s", ErrSubscriptionHeld, e.Code, e.RetryAfter.Round(time.Second))
}

// Unwrap makes every SubscriptionHeldError match [ErrSubscriptionHeld].
func (e *SubscriptionHeldError) Unwrap() error { return ErrSubscriptionHeld }

// Delivery says how a quote reached the client.
type Delivery string

const (
	// DeliveryStream is a line held by subscription.
	DeliveryStream Delivery = "stream"
	// DeliverySnapshot is one answer to a snapshot request.
	DeliverySnapshot Delivery = "snapshot"
)

// Quote is one instrument's quote. A streaming line carries no bid or ask:
// OpenD's basic quote has neither, only a snapshot does, so zero means absent
// on a stream line.
type Quote struct {
	Code      string
	Name      string
	Delivery  Delivery
	Last      float64
	Bid       float64
	Ask       float64
	BidSize   int64
	AskSize   int64
	Open      float64
	High      float64
	Low       float64
	PrevClose float64
	Volume    int64
	Turnover  float64
	LotSize   int32
	Suspended bool
	// At is OpenD's update time for the last price.
	At time.Time
}

// markets maps a code's prefix to OpenD's quote market. The prefixes are the
// public client's, one per market.
var markets = map[string]qotcommon.QotMarket{
	"HK": qotcommon.QotMarket_QotMarket_HK_Security,
	"US": qotcommon.QotMarket_QotMarket_US_Security,
	"SH": qotcommon.QotMarket_QotMarket_CNSH_Security,
	"SZ": qotcommon.QotMarket_QotMarket_CNSZ_Security,
	"SG": qotcommon.QotMarket_QotMarket_SG_Security,
	"JP": qotcommon.QotMarket_QotMarket_JP_Security,
	"AU": qotcommon.QotMarket_QotMarket_AU_Security,
	"MY": qotcommon.QotMarket_QotMarket_MY_Security,
	"CA": qotcommon.QotMarket_QotMarket_CA_Security,
}

// security splits a code into OpenD's security message.
func security(code string) (*qotcommon.Security, error) {
	prefix, symbol, ok := strings.Cut(code, ".")
	market, known := markets[prefix]
	if !ok || !known || symbol == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidCode, code)
	}
	return &qotcommon.Security{Market: new(int32(market)), Code: new(symbol)}, nil
}

func securities(codes []string) ([]*qotcommon.Security, error) {
	out := make([]*qotcommon.Security, 0, len(codes))
	for _, code := range codes {
		sec, err := security(code)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, nil
}

// codeOf joins OpenD's security message back into a code.
func codeOf(sec *qotcommon.Security) string {
	for prefix, market := range markets {
		if int32(market) == sec.GetMarket() {
			return prefix + "." + sec.GetCode()
		}
	}
	return sec.GetCode()
}

func timeFromSeconds(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	whole := int64(seconds)
	return time.Unix(whole, int64((seconds-float64(whole))*1e9)).UTC()
}

func subscribeRequest(secs []*qotcommon.Security) *qotsub.Request {
	return &qotsub.Request{C2S: &qotsub.C2S{
		SecurityList:     secs,
		SubTypeList:      []int32{int32(qotcommon.SubType_SubType_Basic)},
		IsSubOrUnSub:     new(true),
		IsRegOrUnRegPush: new(true),
		IsFirstPush:      new(true),
	}}
}

func unsubscribeRequest(secs []*qotcommon.Security) *qotsub.Request {
	return &qotsub.Request{C2S: &qotsub.C2S{
		SecurityList: secs,
		SubTypeList:  []int32{int32(qotcommon.SubType_SubType_Basic)},
		IsSubOrUnSub: new(false),
	}}
}

// Subscribe takes one streaming line per code for the current session and
// registers the session for its pushes. A line already held costs nothing
// more. It returns the session the lines belong to; a new session holds none.
func (c *Connector) Subscribe(ctx context.Context, codes ...string) (SessionBinding, error) {
	secs, err := securities(codes)
	if err != nil {
		return SessionBinding{}, err
	}
	conn, binding, err := c.quoteSession()
	if err != nil {
		return SessionBinding{}, err
	}
	var resp qotsub.Response
	if err := conn.request(ctx, ProtoQotSub, subscribeRequest(secs), &resp); err != nil {
		return binding, c.subscribeRefused(ctx, conn, codes, err)
	}
	now := c.now()
	c.quotesMu.Lock()
	defer c.quotesMu.Unlock()
	for _, code := range codes {
		if _, held := c.lines[code]; !held {
			c.lines[code] = now
		}
	}
	return binding, nil
}

// subscribeRefused records a refusal and asks OpenD whether its quota is what
// refused it; the fact comes from the quota read, not the refusal's text.
func (c *Connector) subscribeRefused(ctx context.Context, conn *connection, codes []string, err error) error {
	reply, refused := errors.AsType[*Error](err)
	if !refused {
		return err
	}
	if refusal := c.quoteRightRefusal(codes, err); refusal != nil {
		return refusal
	}
	c.quotesMu.Lock()
	c.lastRefusalAt = c.now()
	c.lastRefusal = reply.Message
	c.quotesMu.Unlock()
	load, loadErr := readSubscriptionLoad(ctx, conn)
	if loadErr == nil && load.remaining <= 0 {
		return fmt.Errorf("%w: %d in use: %w", ErrQuotaExhausted, load.used, reply)
	}
	return err
}

// Unsubscribe releases lines the session holds. A line inside its documented
// minimum hold is refused with [SubscriptionHeldError] before anything is
// sent, and the caller releases it later; nothing is sent for a line this
// session never took.
func (c *Connector) Unsubscribe(ctx context.Context, codes ...string) error {
	now := c.now()
	c.quotesMu.Lock()
	release := make([]string, 0, len(codes))
	for _, code := range codes {
		since, held := c.lines[code]
		if !held {
			continue
		}
		if age := now.Sub(since); age < SubscriptionMinimumHold {
			c.quotesMu.Unlock()
			return &SubscriptionHeldError{Code: code, RetryAfter: SubscriptionMinimumHold - age}
		}
		release = append(release, code)
	}
	c.quotesMu.Unlock()
	if len(release) == 0 {
		return nil
	}
	secs, err := securities(release)
	if err != nil {
		return err
	}
	conn, err := c.session()
	if err != nil {
		return err
	}
	var resp qotsub.Response
	if err := conn.request(ctx, ProtoQotSub, unsubscribeRequest(secs), &resp); err != nil {
		return err
	}
	c.quotesMu.Lock()
	defer c.quotesMu.Unlock()
	for _, code := range release {
		delete(c.lines, code)
		delete(c.quotes, code)
	}
	return nil
}

// quoteSession is the open connection and its binding.
func (c *Connector) quoteSession() (*connection, SessionBinding, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conn == nil {
		return nil, SessionBinding{}, ErrNotConnected
	}
	return c.conn, SessionBinding{Epoch: c.epoch}, nil
}

// Lines lists the codes the session holds streaming lines for.
func (c *Connector) Lines() map[string]time.Time {
	c.quotesMu.Lock()
	defer c.quotesMu.Unlock()
	return maps.Clone(c.lines)
}

// QuoteCache is the latest pushed quote of every streaming line.
func (c *Connector) QuoteCache() map[string]Quote {
	c.quotesMu.Lock()
	defer c.quotesMu.Unlock()
	return maps.Clone(c.quotes)
}

func quoteFromBasic(row *qotcommon.BasicQot) Quote {
	return Quote{
		Code:      codeOf(row.GetSecurity()),
		Name:      row.GetName(),
		Delivery:  DeliveryStream,
		Last:      row.GetCurPrice(),
		Open:      row.GetOpenPrice(),
		High:      row.GetHighPrice(),
		Low:       row.GetLowPrice(),
		PrevClose: row.GetLastClosePrice(),
		Volume:    row.GetVolume(),
		Turnover:  row.GetTurnover(),
		Suspended: row.GetIsSuspended(),
		At:        timeFromSeconds(row.GetUpdateTimestamp()),
	}
}

// BasicQuotes reads the current quote of lines the session has subscribed;
// OpenD refuses an instrument without a line.
func (c *Connector) BasicQuotes(ctx context.Context, codes ...string) ([]Quote, error) {
	conn, err := c.session()
	if err != nil {
		return nil, err
	}
	return readBasicQuotes(ctx, conn, codes)
}

func basicQuotesRequest(secs []*qotcommon.Security) *qotgetbasicqot.Request {
	return &qotgetbasicqot.Request{C2S: &qotgetbasicqot.C2S{SecurityList: secs}}
}

func readBasicQuotes(ctx context.Context, conn *connection, codes []string) ([]Quote, error) {
	secs, err := securities(codes)
	if err != nil {
		return nil, err
	}
	var resp qotgetbasicqot.Response
	if err := conn.request(ctx, ProtoQotGetBasicQot, basicQuotesRequest(secs), &resp); err != nil {
		return nil, err
	}
	rows := resp.GetS2C().GetBasicQotList()
	quotes := make([]Quote, 0, len(rows))
	for _, row := range rows {
		quotes = append(quotes, quoteFromBasic(row))
	}
	return quotes, nil
}

func quoteFromSnapshot(row *qotgetsecuritysnapshot.Snapshot) Quote {
	basic := row.GetBasic()
	return Quote{
		Code:      codeOf(basic.GetSecurity()),
		Name:      basic.GetName(),
		Delivery:  DeliverySnapshot,
		Last:      basic.GetCurPrice(),
		Bid:       basic.GetBidPrice(),
		Ask:       basic.GetAskPrice(),
		BidSize:   basic.GetBidVol(),
		AskSize:   basic.GetAskVol(),
		Open:      basic.GetOpenPrice(),
		High:      basic.GetHighPrice(),
		Low:       basic.GetLowPrice(),
		PrevClose: basic.GetLastClosePrice(),
		Volume:    basic.GetVolume(),
		Turnover:  basic.GetTurnover(),
		LotSize:   basic.GetLotSize(),
		Suspended: basic.GetIsSuspend(),
		At:        timeFromSeconds(basic.GetUpdateTimestamp()),
	}
}

func snapshotRequest(secs []*qotcommon.Security) *qotgetsecuritysnapshot.Request {
	return &qotgetsecuritysnapshot.Request{C2S: &qotgetsecuritysnapshot.C2S{SecurityList: secs}}
}

// Snapshots reads any number of instruments, batched to OpenD's limit per
// request; each batch spends one request of the 30-second window. When the
// window is spent partway, the batches already read are returned with the
// [PacedError], so a caller keeps what it got.
func (c *Connector) Snapshots(ctx context.Context, codes ...string) ([]Quote, error) {
	conn, err := c.session()
	if err != nil {
		return nil, err
	}
	quotes := make([]Quote, 0, len(codes))
	var refusals []error
	for _, group := range codesByMarket(codes) {
		rows, err := readSnapshots(ctx, conn, group)
		quotes = append(quotes, rows...)
		if err == nil {
			continue
		}
		refusal := c.quoteRightRefusal(group, err)
		if refusal == nil {
			return quotes, err
		}
		refusals = append(refusals, refusal)
	}
	return quotes, errors.Join(refusals...)
}

// codesByMarket groups codes by market, in the order each market first appears.
// OpenD refuses a whole quote request when one code's market has no right, so one
// market's refusal must not reach the others.
func codesByMarket(codes []string) [][]string {
	var groups [][]string
	at := map[string]int{}
	for _, code := range codes {
		market := codeMarket(code)
		i, seen := at[market]
		if !seen {
			i = len(groups)
			at[market] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], code)
	}
	return groups
}

func codeMarket(code string) string {
	market, _, _ := strings.Cut(code, ".")
	return market
}

// quoteRightRefusal reads a refused quote request as a missing right, nil when it
// is anything else. OpenD gives no error code, so this takes the rights it reported
// where it has them and its message otherwise, in either of OpenD's languages.
func (c *Connector) quoteRightRefusal(codes []string, err error) *QuoteRightError {
	reply, refused := errors.AsType[*Error](err)
	if !refused || reply.RetType != RetTypeFailed || len(codes) == 0 {
		return nil
	}
	// OpenD names the code it refused; a request can hold several markets.
	market := codeMarket(codes[0])
	for _, code := range codes {
		if strings.Contains(reply.Message, code) {
			market = codeMarket(code)
			break
		}
	}
	right := c.Health().QuoteRights[market]
	lower := strings.ToLower(reply.Message)
	if right != "No" && !strings.Contains(lower, "permission") && !strings.Contains(reply.Message, "权限") {
		return nil
	}
	return &QuoteRightError{Market: market, Right: right, Codes: codes, Message: reply.Message}
}

func readSnapshots(ctx context.Context, conn *connection, codes []string) ([]Quote, error) {
	secs, err := securities(codes)
	if err != nil {
		return nil, err
	}
	limit, _ := LimitFor(ProtoQotGetSecuritySnapshot)
	quotes := make([]Quote, 0, len(codes))
	for start := 0; start < len(secs); start += limit.PerRequest {
		var resp qotgetsecuritysnapshot.Response
		batch := secs[start:min(start+limit.PerRequest, len(secs))]
		if err := conn.request(ctx, ProtoQotGetSecuritySnapshot, snapshotRequest(batch), &resp); err != nil {
			return quotes, err
		}
		for _, row := range resp.GetS2C().GetSnapshotList() {
			quotes = append(quotes, quoteFromSnapshot(row))
		}
	}
	return quotes, nil
}

// handleBasicQuotePush takes one streaming update from the read loop.
func (c *Connector) handleBasicQuotePush(conn *connection, frame Frame) {
	var resp qotupdatebasicqot.Response
	if err := proto.Unmarshal(frame.Body, &resp); err != nil {
		conn.close(fmt.Errorf("%w: Qot_UpdateBasicQot body: %v", ErrFrame, err))
		return
	}
	c.quotesMu.Lock()
	defer c.quotesMu.Unlock()
	for _, row := range resp.GetS2C().GetBasicQotList() {
		quote := quoteFromBasic(row)
		if _, held := c.lines[quote.Code]; held {
			c.quotes[quote.Code] = quote
		}
	}
}
