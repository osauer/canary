package futu

import (
	"context"
	"time"

	qotgetsubinfo "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getsubinfo"
	qotrequesthistoryklquota "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_requesthistoryklquota"
)

// Load is OpenD's quota as this client last read it. Every figure is OpenD's
// own and OpenD-wide: another client on the same OpenD spends the same quota,
// so a refusal is reported and never predicted from these numbers.
type Load struct {
	SubscriptionsUsed      int
	SubscriptionsRemaining int
	SubscriptionQuota      int
	HistoryUsed            int
	HistoryRemaining       int
	HistoryQuota           int
	LastRefusalAt          time.Time
	LastRefusal            string
}

// subscriptionLoad is one Qot_GetSubInfo answer.
type subscriptionLoad struct {
	used, remaining int
}

func subInfoRequest() *qotgetsubinfo.Request {
	return &qotgetsubinfo.Request{C2S: &qotgetsubinfo.C2S{IsReqAllConn: new(false)}}
}

func readSubscriptionLoad(ctx context.Context, conn *connection) (subscriptionLoad, error) {
	var resp qotgetsubinfo.Response
	if err := conn.request(ctx, ProtoQotGetSubInfo, subInfoRequest(), &resp); err != nil {
		return subscriptionLoad{}, err
	}
	return subscriptionLoad{used: int(resp.GetS2C().GetTotalUsedQuota()), remaining: int(resp.GetS2C().GetRemainQuota())}, nil
}

func historyQuotaRequest() *qotrequesthistoryklquota.Request {
	return &qotrequesthistoryklquota.Request{C2S: &qotrequesthistoryklquota.C2S{BGetDetail: new(false)}}
}

func readHistoryQuota(ctx context.Context, conn *connection) (used, remaining int, err error) {
	var resp qotrequesthistoryklquota.Response
	if err := conn.request(ctx, ProtoQotRequestHistoryQuota, historyQuotaRequest(), &resp); err != nil {
		return 0, 0, err
	}
	return int(resp.GetS2C().GetUsedQuota()), int(resp.GetS2C().GetRemainQuota()), nil
}

// RefreshLoad reads both quotas from OpenD and returns the result.
func (c *Connector) RefreshLoad(ctx context.Context) (Load, error) {
	conn, err := c.session()
	if err != nil {
		return c.Load(), err
	}
	subs, err := readSubscriptionLoad(ctx, conn)
	if err != nil {
		return c.Load(), err
	}
	used, remaining, err := readHistoryQuota(ctx, conn)
	if err != nil {
		return c.Load(), err
	}
	c.quotesMu.Lock()
	c.load = Load{
		SubscriptionsUsed:      subs.used,
		SubscriptionsRemaining: subs.remaining,
		SubscriptionQuota:      subs.used + subs.remaining,
		HistoryUsed:            used,
		HistoryRemaining:       remaining,
		HistoryQuota:           used + remaining,
	}
	c.quotesMu.Unlock()
	return c.Load(), nil
}

// Load is the last quota read, with the pushed figures on top and the last
// refusal. It sends nothing.
func (c *Connector) Load() Load {
	health := c.Health()
	c.quotesMu.Lock()
	defer c.quotesMu.Unlock()
	load := c.load
	if health.SubscriptionQuota > 0 {
		load.SubscriptionQuota = health.SubscriptionQuota
		load.HistoryQuota = health.HistoryQuota
	}
	// OpenD pushes the quota and the spend as two messages; a spend that has not
	// arrived yet does not read as zero.
	if health.SpendPushed {
		load.SubscriptionsUsed = health.SubscriptionsUsed
		load.HistoryUsed = health.HistoryUsed
	}
	if load.SubscriptionQuota > 0 {
		load.SubscriptionsRemaining = load.SubscriptionQuota - load.SubscriptionsUsed
	}
	if load.HistoryQuota > 0 {
		load.HistoryRemaining = load.HistoryQuota - load.HistoryUsed
	}
	load.LastRefusalAt = c.lastRefusalAt
	load.LastRefusal = c.lastRefusal
	return load
}
