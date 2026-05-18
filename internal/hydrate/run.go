package hydrate

import (
	"context"
	"fmt"
	"log"
)

// Config bundles the inputs to Run. Kept separate from the per-page fetch
// args so callers (the relay's startup hook, the CLI) don't need to
// know about the Typesense REST surface.
type Config struct {
	TSHost       string
	TSAPIKey     string
	TSCollection string
	PageSize     int // Typesense per_page; defaults to 250 if zero
}

// Logger is a minimal subset of log.Logger so callers can plug in their own
// (the relay uses fmt.Printf at startup; the CLI uses the std log package).
// nil = use stdlib log.
type Logger interface {
	Printf(format string, args ...any)
}

// Run executes both reconciliation passes against the Typesense collection
// described in cfg, against the given sink (canonical save pass) and store
// (verify pass). Returns the aggregate stats from both passes.
//
// Pass 1 walks every TS doc, decodes eventRaw, saves into the sink. Pass 2
// re-walks and cross-checks that the store has a row keyed at every
// TS.eventID; on miss, parses the eventRaw, overrides its ID, and re-saves.
//
// Errors from a single page abort the whole run (returned to the caller).
// Per-doc errors do not — they are aggregated into Stats / VerifyStats so
// the caller can see how much of the corpus succeeded.
//
// store may be nil to skip the verify pass (used by the CLI's --dry-run
// mode, which has no real store to verify against).
func Run(ctx context.Context, cfg Config, sink EventSink, store EventStore, logger Logger) (Stats, VerifyStats, error) {
	if cfg.PageSize <= 0 {
		cfg.PageSize = 250
	}
	if logger == nil {
		logger = stdLogger{}
	}

	var total Stats
	page := 1
	for {
		docs, err := FetchPage(ctx, cfg.TSHost, cfg.TSAPIKey, cfg.TSCollection, page, cfg.PageSize)
		if err != nil {
			return total, VerifyStats{}, fmt.Errorf("fetch page %d: %w", page, err)
		}
		if len(docs) == 0 {
			break
		}
		raws := make([]string, 0, len(docs))
		for _, d := range docs {
			raws = append(raws, d.EventRaw)
		}
		s := HydrateBatch(raws, sink)
		total.Scanned += s.Scanned
		total.Saved += s.Saved
		total.AlreadyPresent += s.AlreadyPresent
		total.ParseErrors += s.ParseErrors
		total.SaveErrors += s.SaveErrors
		logger.Printf("hydrate page %d: scanned=%d saved=%d already=%d parseErr=%d saveErr=%d (running: scanned=%d saved=%d)",
			page, s.Scanned, s.Saved, s.AlreadyPresent, s.ParseErrors, s.SaveErrors, total.Scanned, total.Saved)
		if len(docs) < cfg.PageSize {
			break
		}
		page++
	}

	if store == nil {
		return total, VerifyStats{}, nil
	}

	var verifyTotal VerifyStats
	logger.Printf("hydrate: starting verify pass: re-walk Typesense, check store has each TS.eventID")
	page = 1
	for {
		docs, err := FetchPage(ctx, cfg.TSHost, cfg.TSAPIKey, cfg.TSCollection, page, cfg.PageSize)
		if err != nil {
			return total, verifyTotal, fmt.Errorf("verify: fetch page %d: %w", page, err)
		}
		if len(docs) == 0 {
			break
		}
		ps := VerifyStats{}
		for _, d := range docs {
			ps.Scanned++
			if d.EventID == "" {
				ps.ParseErrors++
				continue
			}
			outcome, id, verr := VerifyOne(d.EventID, d.EventRaw, store)
			switch outcome {
			case VerifyOK:
				ps.OK++
			case VerifyResaved:
				ps.Mismatches++
				ps.Resaved++
				logger.Printf("hydrate verify: WARN ts_eventID=%s not in store; resaved under TS.eventID", id.Hex())
			case VerifyAlreadyResave:
				ps.Mismatches++
				logger.Printf("hydrate verify: WARN ts_eventID=%s missing then dup on resave (race?)", id.Hex())
			case VerifyParseError:
				ps.ParseErrors++
				if verr != nil {
					logger.Printf("hydrate verify: parse error for ts_eventID=%s: %v", d.EventID, verr)
				}
			case VerifySaveError:
				ps.Mismatches++
				ps.SaveErrors++
				logger.Printf("hydrate verify: save error for ts_eventID=%s: %v", id.Hex(), verr)
			}
		}
		verifyTotal.Scanned += ps.Scanned
		verifyTotal.OK += ps.OK
		verifyTotal.Mismatches += ps.Mismatches
		verifyTotal.Resaved += ps.Resaved
		verifyTotal.ParseErrors += ps.ParseErrors
		verifyTotal.SaveErrors += ps.SaveErrors
		logger.Printf("hydrate verify page %d: scanned=%d ok=%d mismatches=%d resaved=%d parseErr=%d saveErr=%d (running: mismatches=%d resaved=%d)",
			page, ps.Scanned, ps.OK, ps.Mismatches, ps.Resaved, ps.ParseErrors, ps.SaveErrors,
			verifyTotal.Mismatches, verifyTotal.Resaved)
		if len(docs) < cfg.PageSize {
			break
		}
		page++
	}
	return total, verifyTotal, nil
}

// stdLogger forwards Printf calls to the standard log package. Used as the
// default when Run is called with logger=nil.
type stdLogger struct{}

func (stdLogger) Printf(format string, args ...any) { log.Printf(format, args...) }
