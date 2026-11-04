package futu

import (
	"time"

	"google.golang.org/protobuf/proto"

	qotcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/qot_common"
	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
)

// FixtureConfig is the client identity every frame fixture is recorded with.
var FixtureConfig = Config{ClientID: "canary-fixture", ClientVersion: 333}

// The instants the history fixture asks between, in the market's own time.
const (
	fixtureHistoryFrom = "2026-10-26 00:00:00"
	fixtureHistoryTo   = "2026-10-30 23:59:59"
)

func fixtureHeader(account uint64, env trdcommon.TrdEnv) *trdcommon.TrdHeader {
	return &trdcommon.TrdHeader{
		TrdEnv:    new(int32(env)),
		AccID:     new(account),
		TrdMarket: new(int32(trdcommon.TrdMarket_TrdMarket_HK)),
	}
}

func fixtureSimHeader() *trdcommon.TrdHeader {
	return fixtureHeader(1000001, trdcommon.TrdEnv_TrdEnv_Simulate)
}

func fixtureRealHeader() *trdcommon.TrdHeader {
	return fixtureHeader(1000002, trdcommon.TrdEnv_TrdEnv_Real)
}

func fixtureSecurities(codes ...string) []*qotcommon.Security {
	secs, err := securities(codes)
	if err != nil {
		panic(err)
	}
	return secs
}

// FixtureRequest renders the request of one scripted exchange exactly as the
// client's own builders do, with the serial the client would give it.
func FixtureRequest(name string, serial uint32, now time.Time) []byte {
	var (
		protoID uint32
		message proto.Message
	)
	switch name {
	case "init_connect":
		protoID, message = ProtoInitConnect, initConnectRequest(FixtureConfig)
	case "keep_alive":
		protoID, message = ProtoKeepAlive, keepAliveRequest(now)
	case "global_state":
		protoID, message = ProtoGetGlobalState, globalStateRequest()
	case "qot_sub":
		protoID, message = ProtoQotSub, subscribeRequest(fixtureSecurities("HK.00700"))
	case "qot_basic":
		protoID, message = ProtoQotGetBasicQot, basicQuotesRequest(fixtureSecurities("HK.00700"))
	case "qot_snapshot":
		protoID, message = ProtoQotGetSecuritySnapshot, snapshotRequest(fixtureSecurities("HK.00700", "US.AAPL"))
	case "qot_sub_info":
		protoID, message = ProtoQotGetSubInfo, subInfoRequest()
	case "qot_history":
		sec := fixtureSecurities("HK.00700")[0]
		protoID, message = ProtoQotRequestHistoryKL, historyBarsRequest(sec, qotcommon.KLType_KLType_Day, fixtureHistoryFrom, fixtureHistoryTo, nil)
	case "qot_history_quota":
		protoID, message = ProtoQotRequestHistoryQuota, historyQuotaRequest()
	case "qot_static":
		protoID, message = ProtoQotGetStaticInfo, staticInfoRequest(fixtureSecurities("HK.00700"))
	case "trd_funds":
		protoID, message = ProtoTrdGetFunds, fundsRequest(fixtureSimHeader(), false)
	case "trd_positions":
		protoID, message = ProtoTrdGetPositionList, positionsRequest(fixtureSimHeader(), false)
	case "trd_orders":
		protoID, message = ProtoTrdGetOrderList, orderListRequest(fixtureSimHeader(), false)
	case "trd_fills":
		protoID, message = ProtoTrdGetOrderFillList, fillListRequest(fixtureSimHeader(), false)
	case "trd_max":
		protoID, message = ProtoTrdGetMaxTrdQtys, maxQuantitiesRequest(fixtureSimHeader(), "00700", trdcommon.OrderType_OrderType_Normal, 412.6)
	case "trd_max_cash_wrong_env":
		protoID, message = ProtoTrdGetMaxTrdQtys, maxQuantitiesRequest(fixtureHeader(1000001, trdcommon.TrdEnv_TrdEnv_Real), "00700", trdcommon.OrderType_OrderType_Normal, 412.6)
	case "trd_max_cash":
		protoID, message = ProtoTrdGetMaxTrdQtys, maxQuantitiesRequest(fixtureRealHeader(), "02800", trdcommon.OrderType_OrderType_Normal, 23.44)
	default:
		panic("no fixture request named " + name)
	}
	body, err := marshalDeterministic(message)
	if err != nil {
		panic(err)
	}
	return EncodeFrame(Frame{ProtoID: protoID, Serial: serial, Body: body})
}
