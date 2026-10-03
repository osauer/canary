package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const stateKindWatchlist = "watchlist"
const watchlistReceiptType = "watchlist_saved"

type watchlistDocument struct {
	Version int                     `json:"version"`
	Symbols []rpc.WatchlistContract `json:"symbols"`
}

// All canonical terms, including the revision fence and operation, are immutable.
type watchlistMutation struct {
	Operation        string                  `json:"operation"`
	Symbols          []rpc.WatchlistContract `json:"symbols"`
	Contract         *rpc.WatchlistContract  `json:"contract,omitempty"`
	Symbol           string                  `json:"symbol,omitempty"`
	ExpectedRevision int64                   `json:"expected_revision"`
	RequestID        string                  `json:"request_id"`
}

type watchlistReceipt struct {
	Version       int               `json:"version"`
	Request       watchlistMutation `json:"request"`
	RequestDigest string            `json:"request_digest"`
	SavedRevision int64             `json:"saved_revision"`
}

func decodeWatchlistMutation(req *rpc.Request) (watchlistMutation, error) {
	in := watchlistMutation{Operation: req.Method}
	if len(req.Params) > 16384 {
		return in, errBadRequest("watchlist request exceeds 16 KiB")
	}
	var revision *int64
	var err error
	switch req.Method {
	case rpc.MethodWatchlistReplace:
		var wire struct {
			Symbols          []rpc.WatchlistContract `json:"symbols"`
			ExpectedRevision *int64                  `json:"expected_revision"`
			RequestID        string                  `json:"request_id"`
		}
		err = decodeStrictPlatformSettingsJSON(req.Params, &wire)
		if err == nil {
			in.Symbols, err = rpc.NormalizeWatchlistSymbols(wire.Symbols)
		}
		revision, in.RequestID = wire.ExpectedRevision, wire.RequestID
	case rpc.MethodWatchlistAdd:
		var wire struct {
			Contract         *rpc.WatchlistContract `json:"contract"`
			ExpectedRevision *int64                 `json:"expected_revision"`
			RequestID        string                 `json:"request_id"`
		}
		err = decodeStrictPlatformSettingsJSON(req.Params, &wire)
		if err == nil && wire.Contract == nil {
			err = fmt.Errorf("contract required")
		}
		if err == nil {
			var c rpc.WatchlistContract
			c, err = rpc.NormalizeWatchlistContract(*wire.Contract)
			in.Contract = &c
		}
		revision, in.RequestID = wire.ExpectedRevision, wire.RequestID
	case rpc.MethodWatchlistRemove:
		var wire struct {
			Symbol           string `json:"symbol"`
			ExpectedRevision *int64 `json:"expected_revision"`
			RequestID        string `json:"request_id"`
		}
		err = decodeStrictPlatformSettingsJSON(req.Params, &wire)
		if err == nil {
			var c rpc.WatchlistContract
			c, err = rpc.NormalizeWatchlistContract(rpc.WatchlistContract{Symbol: wire.Symbol})
			in.Symbol = c.Symbol
		}
		revision, in.RequestID = wire.ExpectedRevision, wire.RequestID
	default:
		err = fmt.Errorf("unknown watchlist operation")
	}
	if err != nil || revision == nil {
		return in, errBadRequest("invalid watchlist mutation: explicit terms, expected_revision and request_id required")
	}
	in.ExpectedRevision = *revision
	if err = rpc.ValidateWatchlistMutation(in.ExpectedRevision, in.RequestID); err != nil {
		return in, errBadRequest(err.Error())
	}
	return in, nil
}

func watchlistConflict() error {
	return &rpc.Error{Code: rpc.CodeWatchlistConflict, Message: "Watchlist terms or revision changed; refresh before saving"}
}

// No cache, legacy-file fallback, gateway, quote subscription or contract lookup.
func (s *Server) handleWatchlistList(ctx context.Context) (*rpc.Watchlist, error) {
	return s.acceptWatchlist(ctx, "", 0, false)
}

func (s *Server) acceptWatchlist(ctx context.Context, requestID string, savedRevision int64, replay bool) (*rpc.Watchlist, error) {
	if s.coreStore == nil {
		return nil, fmt.Errorf("watchlist durability unavailable")
	}
	out := rpc.Watchlist{Version: 1, Symbols: []rpc.WatchlistContract{}, RequestID: requestID, SavedRevision: savedRevision, Replay: replay}
	err := s.coreStore.WithAcceptedStateDocument(ctx, daemonStateScope, stateKindWatchlist, func(doc corestore.StateDocument, found bool) error {
		if found {
			var data watchlistDocument
			if err := decodeStrictPlatformSettingsJSON(doc.JSON, &data); err != nil || data.Version != 1 || doc.Revision <= 0 {
				return fmt.Errorf("invalid watchlist document")
			}
			normalized, err := rpc.NormalizeWatchlistSymbols(data.Symbols)
			if err != nil || !slices.Equal(normalized, data.Symbols) {
				return fmt.Errorf("invalid watchlist symbols")
			}
			out.Symbols, out.Revision = normalized, doc.Revision
		}
		if savedRevision > out.Revision {
			return fmt.Errorf("watchlist receipt exceeds accepted state")
		}
		out.AsOf = s.orderNow().UTC()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("watchlist durability unconfirmed: %w", err)
	}
	return &out, nil
}

func (s *Server) handleWatchlistMutation(ctx context.Context, req *rpc.Request) (*rpc.Watchlist, error) {
	in, err := decodeWatchlistMutation(req)
	if err != nil {
		return nil, err
	}
	s.watchlistMu.Lock()
	defer s.watchlistMu.Unlock()
	if s.coreStore == nil || !s.coreStore.Health().Ready {
		return nil, fmt.Errorf("watchlist durability unavailable")
	}
	rawRequest, _ := json.Marshal(in)
	digest := sha256.Sum256(rawRequest)
	requestDigest := hex.EncodeToString(digest[:])
	idHash := sha256.Sum256([]byte(in.RequestID))
	eventKey := "watchlist:" + hex.EncodeToString(idHash[:])
	event, found, err := s.coreStore.GetEvent(ctx, daemonStateScope, eventKey)
	if err != nil {
		return nil, fmt.Errorf("watchlist receipt unavailable")
	}
	if found {
		var receipt watchlistReceipt
		if event.Type != watchlistReceiptType || event.Origin != rpc.OrderOriginAgent || event.Action != coreEventActionUpdate || decodeStrictPlatformSettingsJSON(event.PayloadJSON, &receipt) != nil || receipt.Version != 1 || receipt.SavedRevision < 1 {
			return nil, fmt.Errorf("invalid watchlist receipt")
		}
		canonical, _ := json.Marshal(receipt.Request)
		verified := sha256.Sum256(canonical)
		if hex.EncodeToString(verified[:]) != receipt.RequestDigest {
			return nil, fmt.Errorf("watchlist receipt digest mismatch")
		}
		if receipt.RequestDigest != requestDigest {
			return nil, watchlistConflict()
		}
		return s.acceptWatchlist(ctx, in.RequestID, receipt.SavedRevision, true)
	}
	current, err := s.handleWatchlistList(ctx)
	if err != nil {
		return nil, err
	}
	if in.ExpectedRevision != current.Revision {
		return nil, watchlistConflict()
	}
	next := slices.Clone(current.Symbols)
	switch in.Operation {
	case rpc.MethodWatchlistReplace:
		next = in.Symbols
	case rpc.MethodWatchlistAdd:
		index := slices.IndexFunc(next, func(c rpc.WatchlistContract) bool { return c.Symbol == in.Contract.Symbol })
		if index >= 0 {
			if next[index] != *in.Contract {
				return nil, watchlistConflict()
			}
		} else {
			next = append(next, *in.Contract)
		}
	case rpc.MethodWatchlistRemove:
		next = slices.DeleteFunc(next, func(c rpc.WatchlistContract) bool { return c.Symbol == in.Symbol })
	}
	if next == nil {
		next = []rpc.WatchlistContract{}
	}
	next, err = rpc.NormalizeWatchlistSymbols(next)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	changed := current.Revision == 0 || !slices.Equal(current.Symbols, next)
	revision := current.Revision
	if changed {
		revision++
	}
	rawReceipt, _ := json.Marshal(watchlistReceipt{Version: 1, Request: in, RequestDigest: requestDigest, SavedRevision: revision})
	input := corestore.EventInput{ScopeKey: daemonStateScope, EventKey: eventKey, Type: watchlistReceiptType, Action: coreEventActionUpdate, Origin: rpc.OrderOriginAgent, OccurredAt: s.orderNow().UTC(), PayloadJSON: rawReceipt}
	cas := corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindWatchlist, ExpectedRevision: current.Revision}
	if changed {
		cas.JSON, _ = json.Marshal(watchlistDocument{Version: 1, Symbols: next})
		_, _, err = s.coreStore.CompareAndSwapStateDocumentWithEvents(ctx, cas, []corestore.EventInput{input})
	} else {
		_, err = s.coreStore.AppendEventsAtStateRevision(ctx, cas, []corestore.EventInput{input})
	}
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, watchlistConflict()
	}
	if err != nil {
		return nil, fmt.Errorf("watchlist save unconfirmed")
	}
	return s.acceptWatchlist(ctx, in.RequestID, revision, false)
}

func (s *Server) watchlistSubsystemHealth() rpc.SubsystemHealth {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.handleWatchlistList(ctx); err != nil {
		return rpc.SubsystemHealth{Name: "watchlist", Status: "unavailable", Message: "durable watchlist unavailable"}
	}
	return rpc.SubsystemHealth{Name: "watchlist", Status: "ready", Message: "durable owner list; broker-independent"}
}
