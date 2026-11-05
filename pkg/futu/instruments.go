package futu

import (
	"context"
	"strings"

	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
	qotgetstaticinfo "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_getstaticinfo"
)

// Instrument is what OpenD knows about one listed security.
type Instrument struct {
	Code     string
	Name     string
	Type     string
	LotSize  int32
	Listed   string
	Delisted bool
	// USVenue is the US exchange listing this security, as OpenD reports it;
	// empty for a security elsewhere or when OpenD does not say.
	USVenue string
}

// usVenues name the US exchanges a stock lists on.
var usVenues = map[qotcommon.ExchType]string{
	qotcommon.ExchType_ExchType_US_NYSE:   "NYSE",
	qotcommon.ExchType_ExchType_US_Nasdaq: "NASDAQ",
	qotcommon.ExchType_ExchType_US_AMEX:   "AMEX",
	qotcommon.ExchType_ExchType_US_Pink:   "PINK",
}

func staticInfoRequest(secs []*qotcommon.Security) *qotgetstaticinfo.Request {
	return &qotgetstaticinfo.Request{C2S: &qotgetstaticinfo.C2S{SecurityList: secs}}
}

// Instruments looks up instruments by code.
func (c *Connector) Instruments(ctx context.Context, codes ...string) ([]Instrument, error) {
	conn, err := c.session()
	if err != nil {
		return nil, err
	}
	return readInstruments(ctx, conn, codes)
}

func readInstruments(ctx context.Context, conn *connection, codes []string) ([]Instrument, error) {
	secs, err := securities(codes)
	if err != nil {
		return nil, err
	}
	var resp qotgetstaticinfo.Response
	if err := conn.request(ctx, ProtoQotGetStaticInfo, staticInfoRequest(secs), &resp); err != nil {
		return nil, err
	}
	rows := resp.GetS2C().GetStaticInfoList()
	out := make([]Instrument, 0, len(rows))
	for _, row := range rows {
		basic := row.GetBasic()
		out = append(out, Instrument{
			Code:     codeOf(basic.GetSecurity()),
			Name:     basic.GetName(),
			Type:     strings.TrimPrefix(qotcommon.SecurityType(basic.GetSecType()).String(), "SecurityType_"),
			LotSize:  basic.GetLotSize(),
			Listed:   basic.GetListTime(),
			Delisted: basic.GetDelisting(),
			USVenue:  usVenues[qotcommon.ExchType(basic.GetExchType())],
		})
	}
	return out, nil
}
