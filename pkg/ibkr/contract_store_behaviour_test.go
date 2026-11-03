package ibkr

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mdTAuthority is an in-memory ContractCacheAuthority that records what the
// store hands it and answers loads from a scripted payload.
type mdTAuthority struct {
	mu         sync.Mutex
	payload    []byte
	ok         bool
	loadErr    error
	saveErr    error
	saves      int
	observedAt time.Time
}

func (a *mdTAuthority) LoadContractCache() ([]byte, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loadErr != nil {
		return nil, false, a.loadErr
	}
	return a.payload, a.ok, nil
}

func (a *mdTAuthority) SaveContractCache(payload []byte, observedAt time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.saveErr != nil {
		return a.saveErr
	}
	a.payload, a.ok, a.observedAt = payload, true, observedAt
	a.saves++
	return nil
}

func mdTOptionKey(expiry string, strike float64, right string) string {
	return optionContractKey("SYNX", "SYNX", expiry, strike, right)
}

func mdTOption(expiry string, strike float64, right string, conID int) ContractDetailsLite {
	return ContractDetailsLite{Symbol: "SYNX", SecType: "OPT", TradingClass: "SYNX", Expiry: expiry, Strike: strike, Right: right, ConID: conID}
}

func mdTWriteLegacyFile(t *testing.T, dir, raw string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, contractStoreFile), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mdTTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var tmp []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			tmp = append(tmp, e.Name())
		}
	}
	return tmp
}

// A legacy save round-trips through the file: unresolved contracts (zero
// ConID) and options expired before today's New York date never reach disk,
// the membership hash comes back verbatim, and no temp file is left behind.
func TestContractStoreLegacyRoundTripFiltersUnresolvedAndExpired(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	store := NewContractStore(dir)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewContractStore created its directory: %v", err)
	}
	contracts := map[string]ContractDetailsLite{
		"SYNX": {Symbol: "SYNX", ConID: 4242, Exchange: "SMART", PrimaryExch: "ARCA"},
		"ZETA": {Symbol: "ZETA"},
	}
	options := map[string]ContractDetailsLite{
		mdTOptionKey("20991231", 100, "C"): mdTOption("20991231", 100, "C", 7001),
		mdTOptionKey("20200101", 100, "P"): mdTOption("20200101", 100, "P", 7002),
	}
	if err := store.Save(contracts, options, "members-1"); err != nil {
		t.Fatal(err)
	}
	if len(contracts) != 2 || len(options) != 2 {
		t.Fatal("Save mutated its inputs")
	}
	loaded, hash, err := store.Load()
	if err != nil || hash != "members-1" {
		t.Fatalf("Load = hash %q err %v", hash, err)
	}
	if len(loaded) != 1 || loaded["SYNX"].ConID != 4242 || loaded["SYNX"].PrimaryExch != "ARCA" {
		t.Fatalf("loaded contracts = %+v", loaded)
	}
	opts, err := store.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 1 || opts[mdTOptionKey("20991231", 100, "C")].ConID != 7001 {
		t.Fatalf("loaded options = %+v", opts)
	}
	if _, err := os.Stat(filepath.Join(dir, contractStoreFile)); err != nil {
		t.Fatal(err)
	}
	if tmp := mdTTempFiles(t, dir); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
	// A second save replaces the snapshot rather than merging it.
	if err := store.Save(map[string]ContractDetailsLite{"ZETA": {Symbol: "ZETA", ConID: 4343}}, nil, ""); err != nil {
		t.Fatal(err)
	}
	loaded, hash, err = store.Load()
	if err != nil || hash != "" || len(loaded) != 1 || loaded["ZETA"].ConID != 4343 {
		t.Fatalf("second snapshot = %+v hash %q err %v", loaded, hash, err)
	}
	if opts, err = store.LoadOptions(); err != nil || len(opts) != 0 {
		t.Fatalf("plain Save kept options: %+v err %v", opts, err)
	}
}

// Missing, corrupt and newer-than-known files each have one defined
// outcome: a cold cache, an error, a cold cache. A nil store is always cold.
func TestContractStoreColdCorruptAndNewerEnvelopes(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)
	if got, hash, err := store.Load(); got != nil || hash != "" || err != nil {
		t.Fatalf("missing file Load = %v %q %v", got, hash, err)
	}
	if opts, err := store.LoadOptions(); err != nil || opts == nil || len(opts) != 0 {
		t.Fatalf("missing file LoadOptions = %v %v", opts, err)
	}

	mdTWriteLegacyFile(t, dir, `{"version": 3, "contracts": {`)
	if _, _, err := store.Load(); err == nil {
		t.Fatal("corrupt file loaded without error")
	}
	if _, err := store.LoadOptions(); err == nil {
		t.Fatal("corrupt file loaded options without error")
	}

	mdTWriteLegacyFile(t, dir, `{"version": 4, "as_of": "2026-11-03T00:00:00Z", "members_hash": "x", "contracts": {"SYNX": {"Symbol": "SYNX", "ConID": 4242}}, "options": {"SYNX|SYNX|20991231|100|C": {"ConID": 7}}}`)
	if got, hash, err := store.Load(); got != nil || hash != "" || err != nil {
		t.Fatalf("newer envelope Load = %v %q %v, want cold", got, hash, err)
	}
	if opts, err := store.LoadOptions(); err != nil || len(opts) != 0 {
		t.Fatalf("newer envelope LoadOptions = %v %v, want cold", opts, err)
	}

	// An older legacy envelope stays readable without strict validation.
	mdTWriteLegacyFile(t, dir, `{"version": 2, "contracts": {"SYNX": {"Symbol": "SYNX", "ConID": 4242}, "ZETA": {"Symbol": "ZETA"}}}`)
	got, _, err := store.Load()
	if err != nil || len(got) != 2 || got["SYNX"].ConID != 4242 {
		t.Fatalf("older envelope Load = %v %v", got, err)
	}

	var nilStore *ContractStore
	if got, hash, err := nilStore.Load(); got != nil || hash != "" || err != nil {
		t.Fatalf("nil store Load = %v %q %v", got, hash, err)
	}
	if opts, err := nilStore.LoadOptions(); err != nil || opts == nil || len(opts) != 0 {
		t.Fatalf("nil store LoadOptions = %v %v", opts, err)
	}
	if err := nilStore.UseAuthority(&mdTAuthority{}); err == nil {
		t.Fatal("nil store accepted an authority")
	}
}

// Legacy option keys without a trading-class segment are migrated in memory
// to the empty-class spelling; keys of any other shape are skipped, and
// expired legacy options are pruned on read.
func TestContractStoreLoadOptionsMigratesLegacyKeys(t *testing.T) {
	dir := t.TempDir()
	mdTWriteLegacyFile(t, dir, `{"version": 3, "as_of": "2026-11-03T00:00:00Z", "contracts": {}, "options": {
		"SYNX|20991231|100|C": {"Symbol": "SYNX", "ConID": 7001, "Expiry": "20991231"},
		"SYNX|20200101|100|C": {"Symbol": "SYNX", "ConID": 7002, "Expiry": "20200101"},
		"ZETA|ZETA|20991231|50|P": {"Symbol": "ZETA", "ConID": 7003},
		"BAD|KEY": {"Symbol": "BAD", "ConID": 7004},
		"A|B|C|D|E|F": {"Symbol": "A", "ConID": 7005}
	}}`)
	opts, err := NewContractStore(dir).LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 2 {
		t.Fatalf("loaded options = %+v", opts)
	}
	if opts["SYNX||20991231|100|C"].ConID != 7001 {
		t.Fatalf("legacy key not migrated to the empty-class spelling: %+v", opts)
	}
	if opts["ZETA|ZETA|20991231|50|P"].ConID != 7003 {
		t.Fatalf("current key dropped: %+v", opts)
	}
}

// Save refuses any option whose value contradicts its key and leaves the
// previous snapshot in place; fields omitted from the value are filled from
// the key so a later strict load sees a complete tuple.
func TestContractStoreSaveRefusesContradictoryOptionTuples(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)
	good := mdTOptionKey("20991231", 100, "C")
	if err := store.Save(map[string]ContractDetailsLite{"SYNX": {Symbol: "SYNX", ConID: 4242}}, map[string]ContractDetailsLite{good: {ConID: 7001}}, "first"); err != nil {
		t.Fatal(err)
	}
	opts, err := store.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	filled := opts[good]
	if filled.Symbol != "SYNX" || filled.SecType != "OPT" || filled.TradingClass != "SYNX" || filled.Expiry != "20991231" || filled.Strike != 100 || filled.Right != "C" || filled.ConID != 7001 {
		t.Fatalf("key fields not filled into the value: %+v", filled)
	}

	bad := map[string]map[string]ContractDetailsLite{
		"four-part key":          {"SYNX|20991231|100|C": mdTOption("20991231", 100, "C", 7)},
		"zero conid":             {good: mdTOption("20991231", 100, "C", 0)},
		"symbol contradicts":     {good: {Symbol: "ZETA", ConID: 7}},
		"sectype contradicts":    {good: {SecType: "STK", ConID: 7}},
		"class contradicts":      {good: {TradingClass: "OTHER", ConID: 7}},
		"expiry contradicts":     {good: {Expiry: "20991230", ConID: 7}},
		"strike contradicts":     {good: {Strike: 101, ConID: 7}},
		"right contradicts":      {good: {Right: "P", ConID: 7}},
		"zero strike in key":     {mdTOptionKey("20991231", 0, "C"): {ConID: 7}},
		"bad right in key":       {mdTOptionKey("20991231", 100, "X"): {ConID: 7}},
		"bad expiry in key":      {mdTOptionKey("20991301", 100, "C"): {ConID: 7}},
		"non-canonical spelling": {"synx|SYNX|20991231|100|C": {ConID: 7}},
	}
	for name, options := range bad {
		if err := store.Save(nil, options, "second"); err == nil {
			t.Fatalf("%s: Save accepted %+v", name, options)
		}
	}
	loaded, hash, err := store.Load()
	if err != nil || hash != "first" || loaded["SYNX"].ConID != 4242 {
		t.Fatalf("refused saves disturbed the snapshot: %+v %q %v", loaded, hash, err)
	}
	if tmp := mdTTempFiles(t, dir); len(tmp) != 0 {
		t.Fatalf("refused saves left temp files: %v", tmp)
	}
	// Case and whitespace in the value are tolerated when they agree.
	if err := store.Save(nil, map[string]ContractDetailsLite{good: {Symbol: " synx ", SecType: "opt", TradingClass: " synx", Right: "c", ConID: 9}}, ""); err != nil {
		t.Fatalf("agreeing value refused: %v", err)
	}
}

// UseAuthority validates the authority's current payload before switching
// and leaves the legacy backend in force on any failure; once attached,
// loads and saves go to the authority alone.
func TestContractStoreUseAuthorityValidatesBeforeSwitching(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)
	if err := store.UseAuthority(nil); err == nil {
		t.Fatal("nil authority accepted")
	}
	if err := store.UseAuthority(&mdTAuthority{loadErr: errors.New("synthetic load failure")}); err == nil {
		t.Fatal("authority load failure accepted")
	}
	invalid := map[string]string{
		"old version":   `{"version": 2, "as_of": "2026-11-03T00:00:00Z", "contracts": {}}`,
		"zero as_of":    `{"version": 3, "contracts": {}}`,
		"nil contracts": `{"version": 3, "as_of": "2026-11-03T00:00:00Z"}`,
		"unresolved":    `{"version": 3, "as_of": "2026-11-03T00:00:00Z", "contracts": {"ZETA": {"Symbol": "ZETA"}}}`,
		"blank key":     `{"version": 3, "as_of": "2026-11-03T00:00:00Z", "contracts": {" ": {"Symbol": "SYNX", "ConID": 1}}}`,
		"bad option":    `{"version": 3, "as_of": "2026-11-03T00:00:00Z", "contracts": {}, "options": {"SYNX|20991231|100|C": {"ConID": 7}}}`,
		"not json":      `{`,
	}
	for name, raw := range invalid {
		if err := store.UseAuthority(&mdTAuthority{payload: []byte(raw), ok: true}); err == nil {
			t.Fatalf("%s: invalid authority payload accepted", name)
		}
	}
	// Still on the legacy codec: a save lands in the directory.
	if err := store.Save(map[string]ContractDetailsLite{"SYNX": {Symbol: "SYNX", ConID: 4242}}, nil, "legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, contractStoreFile)); err != nil {
		t.Fatalf("legacy backend abandoned after refused authorities: %v", err)
	}

	authority := &mdTAuthority{}
	if err := store.UseAuthority(authority); err != nil {
		t.Fatal(err)
	}
	// The legacy file is neither imported nor read any more.
	if got, hash, err := store.Load(); got != nil || hash != "" || err != nil {
		t.Fatalf("empty authority Load = %v %q %v", got, hash, err)
	}
	if opts, err := store.LoadOptions(); err != nil || len(opts) != 0 {
		t.Fatalf("empty authority LoadOptions = %v %v", opts, err)
	}
	before, err := os.ReadFile(filepath.Join(dir, contractStoreFile))
	if err != nil {
		t.Fatal(err)
	}
	key := mdTOptionKey("20991231", 100, "C")
	if err := store.Save(map[string]ContractDetailsLite{"ZETA": {Symbol: "ZETA", ConID: 4343}, "NONE": {Symbol: "NONE"}}, map[string]ContractDetailsLite{key: {ConID: 7001}}, "authority"); err != nil {
		t.Fatal(err)
	}
	if authority.saves != 1 {
		t.Fatalf("authority saves = %d", authority.saves)
	}
	var env contractCacheFile
	if err := json.Unmarshal(authority.payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.Version != contractStoreVersion || !env.AsOf.Equal(authority.observedAt) || env.MembersHash != "authority" || len(env.Contracts) != 1 || env.Contracts["ZETA"].ConID != 4343 || env.Options[key].Symbol != "SYNX" {
		t.Fatalf("authority payload = %+v observedAt %s", env, authority.observedAt)
	}
	after, err := os.ReadFile(filepath.Join(dir, contractStoreFile))
	if err != nil || string(after) != string(before) {
		t.Fatalf("authority save touched the legacy file: %v", err)
	}
	loaded, hash, err := store.Load()
	if err != nil || hash != "authority" || len(loaded) != 1 || loaded["ZETA"].ConID != 4343 {
		t.Fatalf("authority Load = %+v %q %v", loaded, hash, err)
	}
	opts, err := store.LoadOptions()
	if err != nil || len(opts) != 1 || opts[key].TradingClass != "SYNX" {
		t.Fatalf("authority LoadOptions = %+v %v", opts, err)
	}

	authority.mu.Lock()
	authority.loadErr = errors.New("synthetic outage")
	authority.mu.Unlock()
	if _, _, err := store.Load(); err == nil {
		t.Fatal("authority load failure hidden")
	}
	if _, err := store.LoadOptions(); err == nil {
		t.Fatal("authority load failure hidden from options")
	}
	authority.mu.Lock()
	authority.loadErr, authority.saveErr = nil, errors.New("synthetic write refusal")
	authority.mu.Unlock()
	if err := store.Save(nil, nil, ""); err == nil {
		t.Fatal("authority save failure hidden")
	}
	if _, err := os.Stat(filepath.Join(dir, contractStoreFile+".tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("authority save fell back to a legacy temp file: %v", err)
	}
}

// SaveRetainingOptions carries forward the previous current-version
// snapshot's options, lets incoming tuples win, prunes expired ones, and
// never resurrects options from an older envelope or past a read failure.
func TestContractStoreSaveRetainingOptionsMergesOnlyCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)
	keep := mdTOptionKey("20991231", 100, "C")
	replace := mdTOptionKey("20991231", 110, "C")
	expired := mdTOptionKey("20200101", 100, "C")

	mdTWriteLegacyFile(t, dir, `{"version": 2, "contracts": {}, "options": {"`+keep+`": {"ConID": 1}}}`)
	if err := store.SaveRetainingOptions(nil, map[string]ContractDetailsLite{replace: {ConID: 2}}, ""); err != nil {
		t.Fatal(err)
	}
	opts, err := store.LoadOptions()
	if err != nil || len(opts) != 1 || opts[replace].ConID != 2 {
		t.Fatalf("older envelope options resurrected: %+v %v", opts, err)
	}

	if err := store.Save(nil, map[string]ContractDetailsLite{keep: {ConID: 1}, replace: {ConID: 2}}, ""); err != nil {
		t.Fatal(err)
	}
	// Plant an already-expired option directly so the prune on merge is observable.
	raw, err := os.ReadFile(filepath.Join(dir, contractStoreFile))
	if err != nil {
		t.Fatal(err)
	}
	var env contractCacheFile
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env.Options[expired] = mdTOption("20200101", 100, "C", 3)
	planted, _ := json.Marshal(env)
	mdTWriteLegacyFile(t, dir, string(planted))

	if err := store.SaveRetainingOptions(map[string]ContractDetailsLite{"SYNX": {Symbol: "SYNX", ConID: 4242}}, map[string]ContractDetailsLite{replace: {ConID: 22}}, "h"); err != nil {
		t.Fatal(err)
	}
	opts, err = store.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(opts) != 2 || opts[keep].ConID != 1 || opts[replace].ConID != 22 {
		t.Fatalf("merge = %+v", opts)
	}
	if _, found := opts[expired]; found {
		t.Fatal("expired option survived the merge")
	}

	mdTWriteLegacyFile(t, dir, `{"version": 3, "contracts": {`)
	if err := store.SaveRetainingOptions(nil, nil, ""); err == nil {
		t.Fatal("unreadable previous snapshot overwritten silently")
	}

	authority := &mdTAuthority{}
	fresh := NewContractStore(t.TempDir())
	if err := fresh.UseAuthority(authority); err != nil {
		t.Fatal(err)
	}
	if err := fresh.SaveRetainingOptions(nil, map[string]ContractDetailsLite{keep: {ConID: 5}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := fresh.SaveRetainingOptions(nil, map[string]ContractDetailsLite{replace: {ConID: 6}}, ""); err != nil {
		t.Fatal(err)
	}
	opts, err = fresh.LoadOptions()
	if err != nil || len(opts) != 2 || opts[keep].ConID != 5 || opts[replace].ConID != 6 {
		t.Fatalf("authority retain = %+v %v", opts, err)
	}
}

// MembersHash names a set: element order, case and surrounding whitespace do
// not change it, while duplicates and element boundaries do.
func TestMembersHashInvariants(t *testing.T) {
	base := MembersHash([]string{"SYNX", "ZETA", "BRK.B"})
	if len(base) != 16 || strings.ToLower(base) != base || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("hash shape %q", base)
	}
	if got := MembersHash([]string{" brk.b", "zeta ", "Synx"}); got != base {
		t.Fatalf("order/case/space changed the hash: %q vs %q", got, base)
	}
	if got := MembersHash([]string{"SYNX", "SYNX", "ZETA", "BRK.B"}); got == base {
		t.Fatal("duplicate member did not change the hash")
	}
	if MembersHash([]string{"A", "B"}) == MembersHash([]string{"AB"}) {
		t.Fatal("element boundary lost")
	}
	if len(MembersHash(nil)) != 16 {
		t.Fatal("empty set has no hash")
	}
}

// The retired legacy root follows XDG_CACHE_HOME, else $HOME/.cache/ibkr,
// and is never created.
func TestDefaultContractStoreDirHonoursXDGThenHome(t *testing.T) {
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("XDG_CACHE_HOME", xdg)
	got, err := DefaultContractStoreDir()
	if err != nil || got != filepath.Join(xdg, "ibkr") {
		t.Fatalf("XDG dir = %q %v", got, err)
	}
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", home)
	got, err = DefaultContractStoreDir()
	if err != nil || got != filepath.Join(home, ".cache", "ibkr") {
		t.Fatalf("HOME dir = %q %v", got, err)
	}
	if _, err := os.Stat(got); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("DefaultContractStoreDir created %s: %v", got, err)
	}
}

// Seeding the option cache fills only unresolved slots and only with
// resolved entries; lookups and snapshots report exactly what is resolved,
// and a connector without a connection has nothing to seed or report.
func TestOptionContractCacheSeedAndLookupThroughConnector(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	resolved := mdTOptionKey("20991231", 100, "C")
	unresolved := mdTOptionKey("20991231", 110, "C")
	if n := c.SeedOptionContracts(map[string]ContractDetailsLite{resolved: {ConID: 7001}, unresolved: {}}); n != 1 {
		t.Fatalf("seeded %d", n)
	}
	if !c.IsOptionContractCached("synx", "synx", "20991231", 100, "C") {
		t.Fatal("resolved option not reported cached")
	}
	if c.IsOptionContractCached("SYNX", "SYNX", "20991231", 110, "C") {
		t.Fatal("unresolved option reported cached")
	}
	if n := c.SeedOptionContracts(map[string]ContractDetailsLite{resolved: {ConID: 9999}}); n != 0 {
		t.Fatalf("re-seed overwrote a resolved entry (%d)", n)
	}
	snap := c.SnapshotOptionContracts()
	if len(snap) != 1 || snap[resolved].ConID != 7001 {
		t.Fatalf("snapshot = %+v", snap)
	}
	snap[resolved] = ContractDetailsLite{}
	if !c.IsOptionContractCached("SYNX", "SYNX", "20991231", 100, "C") {
		t.Fatal("snapshot is not caller-owned")
	}
	if c.SeedOptionContracts(nil) != 0 {
		t.Fatal("empty seed counted")
	}

	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	if c.SeedOptionContracts(map[string]ContractDetailsLite{resolved: {ConID: 1}}) != 0 || c.IsOptionContractCached("SYNX", "SYNX", "20991231", 100, "C") || c.SnapshotOptionContracts() != nil {
		t.Fatal("connector without a connection reported option cache state")
	}
}

// Concurrent saves and loads on one store never observe a torn file or an
// error, and the final snapshot is one complete write.
func TestContractStoreConcurrentSaveAndLoadStayConsistent(t *testing.T) {
	dir := t.TempDir()
	store := NewContractStore(dir)
	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for w := range 4 {
		wg.Go(func() {
			for i := range 8 {
				conID := 1000 + w*100 + i
				if err := store.Save(map[string]ContractDetailsLite{"SYNX": {Symbol: "SYNX", ConID: conID}}, nil, "members"); err != nil {
					errs <- err
				}
			}
		})
		wg.Go(func() {
			for range 8 {
				loaded, hash, err := store.Load()
				if err != nil {
					errs <- err
					continue
				}
				if loaded != nil && (hash != "members" || loaded["SYNX"].ConID < 1000) {
					errs <- errors.New("torn snapshot observed")
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	loaded, _, err := store.Load()
	if err != nil || loaded["SYNX"].ConID < 1000 {
		t.Fatalf("final snapshot = %+v %v", loaded, err)
	}
	if tmp := mdTTempFiles(t, dir); len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
}
