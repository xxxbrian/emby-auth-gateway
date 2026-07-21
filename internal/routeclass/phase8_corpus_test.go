package routeclass

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const phase8Dir = "testdata/phase8"

// Pinned official inventory metadata (must match PROVENANCE.md and fixtures).
const (
	stableLabel         = "4.9.5.0"
	stableCommit        = "bdd0dd7c0801f6e069dff2795d80cddae6f91791"
	stableContentSHA256 = "aa7faf56902160845758ac4c4c7e337f3ad453de4b9ac5b8646981c86d8f597c"
	stableGitBlob       = "bd3ee511e906ff4ebf8837b5383fea4608ee2b25"
	stablePathCount     = 422
	stableOpCount       = 535

	betaLabel         = "4.10.0.20"
	betaCommit        = "d8a719fdc58e552aa1019fc9b5b24a2a3111eef8"
	betaContentSHA256 = "399f9d2fc6a67ae013dad9b66d2cdad00ca2884f6359c4e4565f57debd119b6f"
	betaGitBlob       = "602b50d30cbc182f8229f12a89150e047090caae"
	betaPathCount     = 428
	betaOpCount       = 543

	officialSourcePath = "Resources/OpenApi/openapi_v3.json"
	officialSource     = "MediaBrowser/Emby.SDK"
)

type phase8SchemaFile struct {
	Schema  string         `json:"$schema"`
	Title   string         `json:"title"`
	Defs    map[string]any `json:"$defs"`
	OneOf   []any          `json:"oneOf"`
	Version any            `json:"schemaVersion"` // not on root schema document
}

type officialProvenance struct {
	Kind             string `json:"kind"`
	Source           string `json:"source"`
	Label            string `json:"label"`
	GitCommit        string `json:"gitCommit"`
	SourcePath       string `json:"sourcePath"`
	ContentSHA256    string `json:"contentSha256"`
	GitBlob          string `json:"gitBlob"`
	OpenAPIVersion   string `json:"openapiVersion"`
	InfoVersion      string `json:"infoVersion"`
	AuthoritativeURL string `json:"authoritativeUrl"`
	RepositoryURL    string `json:"repositoryUrl"`
}

type officialOperation struct {
	Method                       string              `json:"method"`
	Path                         string              `json:"path"`
	PathTemplate                 string              `json:"pathTemplate"`
	OperationID                  string              `json:"operationId"`
	Tags                         []string            `json:"tags"`
	Security                     [][]string          `json:"security"`
	QueryNames                   []string            `json:"queryNames"`
	RequestContentTypes          []string            `json:"requestContentTypes"`
	ResponseContentTypes         []string            `json:"responseContentTypes"`
	ResponseContentTypesByStatus map[string][]string `json:"responseContentTypesByStatus"`
	Ownership                    *string             `json:"ownership"`
	Operation                    *string             `json:"operation"`
	Projection                   *string             `json:"projection"`
	Egress                       *string             `json:"egress"`
	ProvenanceKind               string              `json:"provenanceKind"`
}

type officialInventory struct {
	SchemaVersion  int                 `json:"schemaVersion"`
	Kind           string              `json:"kind"`
	Provenance     officialProvenance  `json:"provenance"`
	PathCount      int                 `json:"pathCount"`
	OperationCount int                 `json:"operationCount"`
	Operations     []officialOperation `json:"operations"`
}

type operationKey struct {
	Method string
	Path   string
}

func loadOfficialInventory(t *testing.T, name string) officialInventory {
	t.Helper()
	path := filepath.Join(phase8Dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var inv officialInventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return inv
}

func TestPhase8SchemaV1Present(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(phase8Dir, "schema-v1.json"))
	if err != nil {
		t.Fatalf("read schema-v1.json: %v", err)
	}
	var schema phase8SchemaFile
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse schema-v1.json: %v", err)
	}
	if schema.Title == "" {
		t.Fatal("schema-v1.json missing title")
	}
	if len(schema.OneOf) == 0 {
		t.Fatal("schema-v1.json missing oneOf document kinds")
	}
	for _, name := range []string{
		"officialInventoryDocument",
		"officialOperationRow",
		"officialProvenance",
		"observedCorpusDocument",
		"observedRouteRecord",
		"supportedRoutesDocument",
		"supportedRouteRecord",
		"provenanceKind",
		"schemaVersion",
	} {
		if _, ok := schema.Defs[name]; !ok {
			t.Fatalf("schema-v1.json missing $defs.%s", name)
		}
	}

	// schemaVersion const must be 1 inside $defs.schemaVersion
	rawDefs, err := os.ReadFile(filepath.Join(phase8Dir, "schema-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(rawDefs, &root); err != nil {
		t.Fatal(err)
	}
	defs, _ := root["$defs"].(map[string]any)
	sv, _ := defs["schemaVersion"].(map[string]any)
	if sv["const"] != float64(1) {
		t.Fatalf("schemaVersion const = %#v, want 1", sv["const"])
	}
	pk, _ := defs["provenanceKind"].(map[string]any)
	enum, _ := pk["enum"].([]any)
	wantKinds := map[string]bool{"official": true, "observed": true, "curated": true, "maintained-client": true}
	if len(enum) != len(wantKinds) {
		t.Fatalf("provenanceKind enum len = %d, want %d", len(enum), len(wantKinds))
	}
	for _, v := range enum {
		s, _ := v.(string)
		if !wantKinds[s] {
			t.Fatalf("unexpected provenanceKind %q", s)
		}
	}
}

func TestPhase8ProvenanceDocPresent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(phase8Dir, "PROVENANCE.md"))
	if err != nil {
		t.Fatalf("read PROVENANCE.md: %v", err)
	}
	text := string(raw)
	for _, needle := range []string{
		stableCommit,
		betaCommit,
		stableContentSHA256,
		betaContentSHA256,
		stableGitBlob,
		betaGitBlob,
		officialSourcePath,
		"plugin.video.emby",
		"4051767ec4fde5075b07cac7cdfab7e1061d9d89",
		"official",
		"observed",
		"curated",
		"treated as route authorization",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("PROVENANCE.md missing %q", needle)
		}
	}
}

func TestPhase8OfficialStableInventory(t *testing.T) {
	inv := loadOfficialInventory(t, "official-stable-4.9.5.0.json")
	assertOfficialInventoryMeta(t, inv, officialInventoryMeta{
		label:         stableLabel,
		commit:        stableCommit,
		contentSHA256: stableContentSHA256,
		gitBlob:       stableGitBlob,
		pathCount:     stablePathCount,
		opCount:       stableOpCount,
	})
	assertOperationsWellFormed(t, inv)
}

func TestPhase8OfficialBetaInventory(t *testing.T) {
	inv := loadOfficialInventory(t, "official-beta-4.10.0.20.json")
	assertOfficialInventoryMeta(t, inv, officialInventoryMeta{
		label:         betaLabel,
		commit:        betaCommit,
		contentSHA256: betaContentSHA256,
		gitBlob:       betaGitBlob,
		pathCount:     betaPathCount,
		opCount:       betaOpCount,
	})
	assertOperationsWellFormed(t, inv)
}

func TestPhase8StableIsSubsetOfBeta(t *testing.T) {
	stable := loadOfficialInventory(t, "official-stable-4.9.5.0.json")
	beta := loadOfficialInventory(t, "official-beta-4.10.0.20.json")
	stableKeys := operationKeySet(stable)
	betaKeys := operationKeySet(beta)
	var missing []operationKey
	for k := range stableKeys {
		if !betaKeys[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) != 0 {
		sortOperationKeys(missing)
		t.Fatalf("stable keys missing from beta (%d): %v", len(missing), missing)
	}
}

func TestPhase8BetaOnlyOperations(t *testing.T) {
	stable := loadOfficialInventory(t, "official-stable-4.9.5.0.json")
	beta := loadOfficialInventory(t, "official-beta-4.10.0.20.json")
	stableKeys := operationKeySet(stable)
	betaKeys := operationKeySet(beta)

	var betaOnly []operationKey
	for k := range betaKeys {
		if !stableKeys[k] {
			betaOnly = append(betaOnly, k)
		}
	}
	sortOperationKeys(betaOnly)

	want := []operationKey{
		{Method: "GET", Path: "/Parties/Messages"},
		{Method: "GET", Path: "/Persons/{Id}/Credits"},
		{Method: "POST", Path: "/Parties/Messages"},
		{Method: "POST", Path: "/Users/{UserId}/CopyData"},
		{Method: "POST", Path: "/Users/{UserId}/HomeSections"},
		{Method: "POST", Path: "/Users/{UserId}/HomeSections/Delete"},
		{Method: "POST", Path: "/Users/{UserId}/HomeSections/Move"},
		{Method: "POST", Path: "/Users/{UserId}/SearchedItems/"},
	}
	if len(betaOnly) != len(want) {
		t.Fatalf("beta-only count = %d, want %d\n got: %v\nwant: %v", len(betaOnly), len(want), betaOnly, want)
	}
	for i := range want {
		if betaOnly[i] != want[i] {
			t.Fatalf("beta-only[%d] = %v, want %v\n full got: %v", i, betaOnly[i], want[i], betaOnly)
		}
	}
}

func TestPhase8OperationKeysSortedUnique(t *testing.T) {
	for _, name := range []string{
		"official-stable-4.9.5.0.json",
		"official-beta-4.10.0.20.json",
	} {
		t.Run(name, func(t *testing.T) {
			inv := loadOfficialInventory(t, name)
			if len(inv.Operations) == 0 {
				t.Fatal("no operations")
			}
			seen := make(map[operationKey]struct{}, len(inv.Operations))
			prev := operationKey{}
			for i, op := range inv.Operations {
				k := operationKey{Method: op.Method, Path: op.PathTemplate}
				if _, ok := seen[k]; ok {
					t.Fatalf("duplicate operation key %v at index %d", k, i)
				}
				seen[k] = struct{}{}
				if i > 0 && !lessOperationKey(prev, k) {
					t.Fatalf("operations not strictly sorted by method then pathTemplate at index %d: %v after %v", i, k, prev)
				}
				prev = k
			}
			if len(seen) != inv.OperationCount {
				t.Fatalf("unique keys = %d, operationCount = %d", len(seen), inv.OperationCount)
			}
		})
	}
}

type officialInventoryMeta struct {
	label         string
	commit        string
	contentSHA256 string
	gitBlob       string
	pathCount     int
	opCount       int
}

func assertOfficialInventoryMeta(t *testing.T, inv officialInventory, meta officialInventoryMeta) {
	t.Helper()
	if inv.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", inv.SchemaVersion)
	}
	if inv.Kind != "official-inventory" {
		t.Fatalf("kind = %q, want official-inventory", inv.Kind)
	}
	p := inv.Provenance
	if p.Kind != "official" {
		t.Fatalf("provenance.kind = %q, want official", p.Kind)
	}
	if p.Source != officialSource {
		t.Fatalf("provenance.source = %q, want %q", p.Source, officialSource)
	}
	if p.Label != meta.label {
		t.Fatalf("provenance.label = %q, want %q", p.Label, meta.label)
	}
	if p.GitCommit != meta.commit {
		t.Fatalf("provenance.gitCommit = %q, want %q", p.GitCommit, meta.commit)
	}
	if p.SourcePath != officialSourcePath {
		t.Fatalf("provenance.sourcePath = %q, want %q", p.SourcePath, officialSourcePath)
	}
	if p.ContentSHA256 != meta.contentSHA256 {
		t.Fatalf("provenance.contentSha256 = %q, want %q", p.ContentSHA256, meta.contentSHA256)
	}
	if p.GitBlob != meta.gitBlob {
		t.Fatalf("provenance.gitBlob = %q, want %q", p.GitBlob, meta.gitBlob)
	}
	if p.InfoVersion != meta.label {
		t.Fatalf("provenance.infoVersion = %q, want %q", p.InfoVersion, meta.label)
	}
	if p.OpenAPIVersion == "" {
		t.Fatal("provenance.openapiVersion empty")
	}
	if p.AuthoritativeURL == "" || p.RepositoryURL == "" {
		t.Fatal("provenance URLs must be non-empty")
	}
	if inv.PathCount != meta.pathCount {
		t.Fatalf("pathCount = %d, want %d", inv.PathCount, meta.pathCount)
	}
	if inv.OperationCount != meta.opCount {
		t.Fatalf("operationCount = %d, want %d", inv.OperationCount, meta.opCount)
	}
	if len(inv.Operations) != meta.opCount {
		t.Fatalf("len(operations) = %d, want %d", len(inv.Operations), meta.opCount)
	}

	// Distinct path templates must match pathCount.
	paths := make(map[string]struct{}, inv.PathCount)
	for _, op := range inv.Operations {
		paths[op.PathTemplate] = struct{}{}
	}
	if len(paths) != inv.PathCount {
		t.Fatalf("distinct path templates = %d, pathCount = %d", len(paths), inv.PathCount)
	}
}

func assertOperationsWellFormed(t *testing.T, inv officialInventory) {
	t.Helper()
	for i, op := range inv.Operations {
		if op.Method == "" {
			t.Fatalf("operations[%d]: empty method", i)
		}
		if op.Path == "" || op.PathTemplate == "" {
			t.Fatalf("operations[%d]: empty path/pathTemplate", i)
		}
		if op.Path != op.PathTemplate {
			t.Fatalf("operations[%d]: path %q != pathTemplate %q", i, op.Path, op.PathTemplate)
		}
		if op.Path[0] != '/' {
			t.Fatalf("operations[%d]: pathTemplate %q must start with /", i, op.PathTemplate)
		}
		if op.ProvenanceKind != "official" {
			t.Fatalf("operations[%d]: provenanceKind = %q, want official", i, op.ProvenanceKind)
		}
		if op.Ownership != nil || op.Operation != nil || op.Projection != nil || op.Egress != nil {
			t.Fatalf("operations[%d]: curated fields must be null in official inventory", i)
		}
		if !isSortedUniqueStrings(op.QueryNames) {
			t.Fatalf("operations[%d]: queryNames not sorted unique: %v", i, op.QueryNames)
		}
		if !isSortedUniqueStrings(op.RequestContentTypes) {
			t.Fatalf("operations[%d]: requestContentTypes not sorted unique: %v", i, op.RequestContentTypes)
		}
		if !isSortedUniqueStrings(op.ResponseContentTypes) {
			t.Fatalf("operations[%d]: responseContentTypes not sorted unique: %v", i, op.ResponseContentTypes)
		}
		// responseContentTypes must equal union of per-status types.
		union := make(map[string]struct{})
		for status, types := range op.ResponseContentTypesByStatus {
			if status == "" {
				t.Fatalf("operations[%d]: empty response status key", i)
			}
			if !isSortedUniqueStrings(types) {
				t.Fatalf("operations[%d]: responseContentTypesByStatus[%s] not sorted unique: %v", i, status, types)
			}
			for _, ct := range types {
				union[ct] = struct{}{}
			}
		}
		if len(union) != len(op.ResponseContentTypes) {
			t.Fatalf("operations[%d]: responseContentTypes len %d != union %d", i, len(op.ResponseContentTypes), len(union))
		}
		for _, ct := range op.ResponseContentTypes {
			if _, ok := union[ct]; !ok {
				t.Fatalf("operations[%d]: responseContentTypes has %q not in per-status union", i, ct)
			}
		}
	}
}

func operationKeySet(inv officialInventory) map[operationKey]bool {
	out := make(map[operationKey]bool, len(inv.Operations))
	for _, op := range inv.Operations {
		out[operationKey{Method: op.Method, Path: op.PathTemplate}] = true
	}
	return out
}

func sortOperationKeys(keys []operationKey) {
	sort.Slice(keys, func(i, j int) bool {
		return lessOperationKey(keys[i], keys[j])
	})
}

func lessOperationKey(a, b operationKey) bool {
	if a.Method != b.Method {
		return a.Method < b.Method
	}
	return a.Path < b.Path
}

func isSortedUniqueStrings(vals []string) bool {
	for i := 1; i < len(vals); i++ {
		if vals[i] <= vals[i-1] {
			return false
		}
	}
	return true
}

// --- Observed Web + curated supported-routes corpus ---

type observedRouteRecord struct {
	Method                 string   `json:"method"`
	Path                   string   `json:"path"`
	PathTemplate           string   `json:"pathTemplate"`
	QueryNames             []string `json:"queryNames"`
	Ownership              *string  `json:"ownership"`
	Operation              *string  `json:"operation"`
	Projection             *string  `json:"projection"`
	Egress                 *string  `json:"egress"`
	ProvenanceKind         string   `json:"provenanceKind"`
	Scenario               string   `json:"scenario"`
	Sequence               int      `json:"sequence"`
	Transport              string   `json:"transport"`
	Status                 int      `json:"status"`
	RequestContentTypes    []string `json:"requestContentTypes"`
	ResponseContentTypes   []string `json:"responseContentTypes"`
	Initiator              string   `json:"initiator"`
	BodyShapeHash          string   `json:"bodyShapeHash"`
	RangeBehavior          string   `json:"rangeBehavior"`
	WebsocketFrameCategory string   `json:"websocketFrameCategory"`
}

type supportedEvidence struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Notes string `json:"notes"`
}

type supportedRouteRecord struct {
	ID                  string              `json:"id"`
	Method              string              `json:"method"`
	Path                string              `json:"path"`
	PathTemplate        string              `json:"pathTemplate"`
	QueryNames          []string            `json:"queryNames"`
	Ownership           string              `json:"ownership"`
	Operation           string              `json:"operation"`
	Projection          string              `json:"projection"`
	Egress              string              `json:"egress"`
	ProvenanceKind      string              `json:"provenanceKind"`
	WorkflowTags        []string            `json:"workflowTags"`
	AuthMode            string              `json:"authMode"`
	ExpectedStatusClass string              `json:"expectedStatusClass"`
	Evidence            []supportedEvidence `json:"evidence"`
}

type supportedRoutesDocument struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Kind          string                 `json:"kind"`
	Provenance    map[string]any         `json:"provenance"`
	Routes        []supportedRouteRecord `json:"routes"`
}

func loadObservedWebStable(t *testing.T) []observedRouteRecord {
	t.Helper()
	path := filepath.Join(phase8Dir, "observed-web-stable.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var records []observedRouteRecord
	sc := bufio.NewScanner(f)
	// Observed lines can be long if many query names accumulate.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec observedRouteRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parse %s line %d: %v", path, lineNo, err)
		}
		records = append(records, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if len(records) == 0 {
		t.Fatalf("%s has no records", path)
	}
	return records
}

func loadSupportedRoutes(t *testing.T) supportedRoutesDocument {
	t.Helper()
	path := filepath.Join(phase8Dir, "supported-routes.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc supportedRoutesDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func TestPhase8ObservedWebStableCorpus(t *testing.T) {
	records := loadObservedWebStable(t)

	forbiddenQueryValueHints := []string{"=", "://", "token=", "api_key=", "password"}
	seqByScenario := map[string]int{}

	for i, rec := range records {
		if rec.ProvenanceKind != "observed" {
			t.Fatalf("records[%d]: provenanceKind = %q, want observed", i, rec.ProvenanceKind)
		}
		if rec.Transport != "http" {
			t.Fatalf("records[%d]: transport = %q, want http", i, rec.Transport)
		}
		if rec.Scenario == "" {
			t.Fatalf("records[%d]: empty scenario", i)
		}
		if rec.Method == "" || rec.Path == "" || rec.PathTemplate == "" {
			t.Fatalf("records[%d]: empty method/path/pathTemplate", i)
		}
		if strings.HasPrefix(rec.PathTemplate, "/emby") || strings.Contains(rec.PathTemplate, "/emby/") {
			t.Fatalf("records[%d]: pathTemplate must not include /emby prefix: %q", i, rec.PathTemplate)
		}
		if strings.HasPrefix(rec.Path, "/emby") || strings.Contains(rec.Path, "/emby/") {
			t.Fatalf("records[%d]: path must not include /emby prefix: %q", i, rec.Path)
		}
		if !strings.HasPrefix(rec.PathTemplate, "/") {
			t.Fatalf("records[%d]: pathTemplate %q must start with /", i, rec.PathTemplate)
		}
		if strings.HasPrefix(strings.ToLower(rec.Path), "/web/") || strings.HasPrefix(strings.ToLower(rec.PathTemplate), "/web/") {
			t.Fatalf("records[%d]: static /web assets must not appear in API JSONL: %q", i, rec.Path)
		}
		if rec.Ownership != nil || rec.Operation != nil || rec.Projection != nil || rec.Egress != nil {
			t.Fatalf("records[%d]: curated fields must be null on raw observed records", i)
		}
		if !isSortedUniqueStrings(rec.QueryNames) {
			t.Fatalf("records[%d]: queryNames not sorted unique: %v", i, rec.QueryNames)
		}
		// Observed fixtures retain names only; reject obvious value-bearing forms.
		for _, q := range rec.QueryNames {
			if strings.Contains(q, "=") || strings.Contains(q, "&") {
				t.Fatalf("records[%d]: queryNames entry looks like a value pair: %q", i, q)
			}
			low := strings.ToLower(q)
			for _, hint := range forbiddenQueryValueHints {
				if strings.Contains(low, hint) && strings.Contains(q, "=") {
					t.Fatalf("records[%d]: queryNames must not contain values (%q)", i, q)
				}
			}
		}
		// Sequence is ordered within each scenario (monotonic non-decreasing from 0).
		prev, seen := seqByScenario[rec.Scenario]
		if !seen {
			if rec.Sequence != 0 {
				t.Fatalf("records[%d]: first sequence for scenario %q = %d, want 0", i, rec.Scenario, rec.Sequence)
			}
		} else if rec.Sequence != prev+1 {
			t.Fatalf("records[%d]: scenario %q sequence = %d, want %d (strict order)", i, rec.Scenario, rec.Sequence, prev+1)
		}
		seqByScenario[rec.Scenario] = rec.Sequence

		if rec.Initiator == "" || rec.BodyShapeHash == "" || rec.RangeBehavior == "" || rec.WebsocketFrameCategory == "" {
			t.Fatalf("records[%d]: initiator/bodyShapeHash/rangeBehavior/websocketFrameCategory must be explicit", i)
		}
		// Placeholders only; do not require specific strings beyond non-empty.
		for _, placeholder := range []string{rec.Initiator, rec.BodyShapeHash, rec.RangeBehavior, rec.WebsocketFrameCategory} {
			if strings.Contains(placeholder, "://") {
				t.Fatalf("records[%d]: unexpected fabricated URL-like placeholder %q", i, placeholder)
			}
		}
	}

	if len(seqByScenario) < 2 {
		t.Fatalf("expected at least login-home and search-detail scenarios, got %v", seqByScenario)
	}
	for _, want := range []string{"login-home", "search-detail"} {
		if _, ok := seqByScenario[want]; !ok {
			t.Fatalf("missing scenario %q", want)
		}
	}
}

func TestPhase8SupportedRoutesCorpus(t *testing.T) {
	doc := loadSupportedRoutes(t)
	if doc.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", doc.SchemaVersion)
	}
	if doc.Kind != "supported-routes" {
		t.Fatalf("kind = %q, want supported-routes", doc.Kind)
	}
	if len(doc.Routes) == 0 {
		t.Fatal("supported-routes.json has no routes")
	}
	if provKind, _ := doc.Provenance["kind"].(string); provKind != "curated" {
		t.Fatalf("provenance.kind = %q, want curated", provKind)
	}

	observed := loadObservedWebStable(t)
	obsKeys := make(map[operationKey]bool, len(observed))
	for _, rec := range observed {
		obsKeys[operationKey{Method: rec.Method, Path: rec.PathTemplate}] = true
	}

	ids := make(map[string]struct{}, len(doc.Routes))
	prev := operationKey{}
	forbiddenOwnership := map[string]bool{
		"Legacy": true, "LegacyProxy": true, "Unclassified": true, "legacy": true, "unclassified": true,
	}
	forbiddenOperation := map[string]bool{
		"Legacy": true, "LegacyProxy": true, "Unclassified": true, "OperationLegacyProxy": true,
	}
	forbiddenProjection := map[string]bool{
		"Legacy": true, "LegacyCompatibility": true, "Unclassified": true,
	}
	forbiddenEgress := map[string]bool{
		"Legacy": true, "legacy": true, "Unclassified": true, "unclassified": true,
	}

	deniedTemplates := []operationKey{
		{Method: "POST", Path: "/Users/{UserId}/HideFromResume/{Id}"},
		{Method: "GET", Path: "/Users/{UserId}/HideFromResume"},
		{Method: "GET", Path: "/ScheduledTasks"},
		// Generic StreamFileName / near-miss media — must stay denied.
		{Method: "GET", Path: "/Videos/{ItemId}/{StreamFileName}"},
		{Method: "GET", Path: "/Videos/{Id}/{StreamFileName}"},
		{Method: "GET", Path: "/Items/{Id}/Images"},
		{Method: "GET", Path: "/Items/{ItemId}/Images"},
		{Method: "GET", Path: "/Plugins"},
		{Method: "GET", Path: "/Packages"},
		{Method: "GET", Path: "/Devices"},
	}

	for i, route := range doc.Routes {
		if route.ID == "" {
			t.Fatalf("routes[%d]: empty id", i)
		}
		if _, dup := ids[route.ID]; dup {
			t.Fatalf("duplicate supported route id %q", route.ID)
		}
		ids[route.ID] = struct{}{}

		if route.ProvenanceKind != "curated" {
			t.Fatalf("routes[%d] %s: provenanceKind = %q, want curated", i, route.ID, route.ProvenanceKind)
		}
		if route.Method == "" || route.PathTemplate == "" {
			t.Fatalf("routes[%d] %s: empty method/pathTemplate", i, route.ID)
		}
		// Reject gateway mount prefix /emby/... but allow the local /embywebsocket route.
		if route.PathTemplate == "/emby" || strings.HasPrefix(route.PathTemplate, "/emby/") {
			t.Fatalf("routes[%d] %s: pathTemplate must not include /emby mount prefix: %q", i, route.ID, route.PathTemplate)
		}
		if !strings.HasPrefix(route.PathTemplate, "/") {
			t.Fatalf("routes[%d] %s: pathTemplate must start with /", i, route.ID)
		}
		if route.Ownership == "" || route.Operation == "" || route.Projection == "" || route.Egress == "" {
			t.Fatalf("routes[%d] %s: ownership/operation/projection/egress must be non-empty curated strings", i, route.ID)
		}
		if forbiddenOwnership[route.Ownership] {
			t.Fatalf("routes[%d] %s: forbidden ownership %q", i, route.ID, route.Ownership)
		}
		if forbiddenOperation[route.Operation] {
			t.Fatalf("routes[%d] %s: forbidden operation %q", i, route.ID, route.Operation)
		}
		if forbiddenProjection[route.Projection] {
			t.Fatalf("routes[%d] %s: forbidden projection %q", i, route.ID, route.Projection)
		}
		if forbiddenEgress[route.Egress] {
			t.Fatalf("routes[%d] %s: forbidden egress %q", i, route.ID, route.Egress)
		}
		if !isSortedUniqueStrings(route.QueryNames) {
			t.Fatalf("routes[%d] %s: queryNames not sorted unique: %v", i, route.ID, route.QueryNames)
		}
		for _, q := range route.QueryNames {
			if strings.EqualFold(q, "X-Emby-Token") || strings.EqualFold(q, "api_key") || strings.EqualFold(q, "Password") {
				t.Fatalf("routes[%d] %s: credential query name %q must not appear in curated queryNames", i, route.ID, q)
			}
			if strings.Contains(q, "=") {
				t.Fatalf("routes[%d] %s: queryNames must be names only, got %q", i, route.ID, q)
			}
		}
		if len(route.Evidence) == 0 {
			t.Fatalf("routes[%d] %s: at least one evidence ref required", i, route.ID)
		}
		for j, ev := range route.Evidence {
			if ev.Kind == "" {
				t.Fatalf("routes[%d] %s evidence[%d]: empty kind", i, route.ID, j)
			}
			if ev.Ref == "" {
				t.Fatalf("routes[%d] %s evidence[%d]: empty ref", i, route.ID, j)
			}
		}

		key := operationKey{Method: route.Method, Path: route.PathTemplate}
		gatewayLocal := false
		for _, ev := range route.Evidence {
			if ev.Ref == "gateway-local-test" || strings.Contains(ev.Ref, "gateway-local-test") {
				gatewayLocal = true
				break
			}
		}
		if !obsKeys[key] && !gatewayLocal {
			t.Fatalf("routes[%d] %s: %v not in observed templates and not marked gateway-local-test", i, route.ID, key)
		}

		// Deterministic sort: method, pathTemplate, id.
		if i > 0 {
			if !lessOperationKey(prev, key) && prev != key {
				t.Fatalf("supported routes not sorted by method/pathTemplate at %d: %v after %v", i, key, prev)
			}
			if prev == key {
				// same method+template allowed only if ids strictly increase (we already unique ids)
				if doc.Routes[i-1].ID >= route.ID {
					t.Fatalf("supported routes with same key not ordered by id at %d", i)
				}
			}
		}
		prev = key
	}

	for _, denied := range deniedTemplates {
		for _, route := range doc.Routes {
			if route.Method == denied.Method && route.PathTemplate == denied.Path {
				t.Fatalf("denied example admitted: %s %s (id=%s)", denied.Method, denied.Path, route.ID)
			}
			// Also reject HideFromResume / ScheduledTasks / generic StreamFileName / Images list.
			pt := route.PathTemplate
			if strings.Contains(pt, "HideFromResume") {
				t.Fatalf("HideFromResume must not be supported: %s", route.ID)
			}
			if strings.Contains(pt, "ScheduledTasks") {
				t.Fatalf("ScheduledTasks must not be supported: %s", route.ID)
			}
			if strings.Contains(pt, "{StreamFileName}") {
				t.Fatalf("generic StreamFileName must not be supported: %s", route.ID)
			}
			if route.Method == "GET" && (pt == "/Items/{Id}/Images" || pt == "/Items/{ItemId}/Images") {
				t.Fatalf("image metadata list must not be supported as binary image: %s", route.ID)
			}
		}
	}

	// Required minimum templates from stable observed + local image/logout + W1 media.
	required := []operationKey{
		{Method: "GET", Path: "/System/Info/Public"},
		{Method: "GET", Path: "/Users/Public"},
		{Method: "GET", Path: "/Branding/Configuration"},
		{Method: "POST", Path: "/Users/AuthenticateByName"},
		{Method: "POST", Path: "/Sessions/Capabilities/Full"},
		{Method: "POST", Path: "/Sessions/Logout"},
		{Method: "GET", Path: "/DisplayPreferences/{Id}"},
		{Method: "GET", Path: "/System/Info"},
		{Method: "GET", Path: "/Users/{UserId}/Views"},
		{Method: "GET", Path: "/Users/{UserId}/Items"},
		{Method: "GET", Path: "/Users/{UserId}/Items/Resume"},
		{Method: "GET", Path: "/Users/{UserId}/Items/Latest"},
		{Method: "GET", Path: "/Users/{UserId}/Items/{ItemId}"},
		{Method: "GET", Path: "/Users/{UserId}"},
		{Method: "GET", Path: "/System/Endpoint"},
		{Method: "POST", Path: "/Items/{ItemId}/PlaybackInfo"},
		{Method: "GET", Path: "/Users/{UserId}/Items/{ItemId}/SpecialFeatures"},
		{Method: "GET", Path: "/Items/{ItemId}/Similar"},
		{Method: "GET", Path: "/Items/{ItemId}/ThemeMedia"},
		{Method: "GET", Path: "/Items/{ItemId}/Images/{ImageType}"},
		// W1 exact media
		{Method: "GET", Path: "/Items/{ItemId}/Download"},
		{Method: "GET", Path: "/Videos/{ItemId}/stream"},
		{Method: "GET", Path: "/Videos/{ItemId}/stream.{Container}"},
		{Method: "GET", Path: "/Videos/{ItemId}/master.m3u8"},
		{Method: "GET", Path: "/Videos/{ItemId}/main.m3u8"},
		{Method: "GET", Path: "/Videos/{ItemId}/hls/{SegmentId}.{SegmentContainer}"},
		{Method: "GET", Path: "/Videos/{ItemId}/hls/{PlaylistId}/{SegmentId}.{SegmentContainer}"},
		{Method: "GET", Path: "/Videos/{ItemId}/hls1/{SegmentId}.{SegmentContainer}"},
		{Method: "GET", Path: "/Videos/{ItemId}/hls1/{PlaylistId}/{SegmentId}.{SegmentContainer}"},
		{Method: "GET", Path: "/Videos/{ItemId}/{MediaSourceId}/Subtitles/{Index}/Stream.{Format}"},
		{Method: "GET", Path: "/Videos/{ItemId}/{MediaSourceId}/Subtitles/{Index}/{StartPositionTicks}/Stream.{Format}"},
		{Method: "GET", Path: "/Items/{ItemId}/{MediaSourceId}/Subtitles/{Index}/Stream.{Format}"},
		{Method: "GET", Path: "/Items/{ItemId}/{MediaSourceId}/Subtitles/{Index}/{StartPositionTicks}/Stream.{Format}"},
		{Method: "GET", Path: "/Audio/{ItemId}/stream"},
		{Method: "GET", Path: "/Audio/{ItemId}/stream.{Container}"},
	}
	have := make(map[operationKey]bool, len(doc.Routes))
	for _, route := range doc.Routes {
		have[operationKey{Method: route.Method, Path: route.PathTemplate}] = true
	}
	for _, want := range required {
		if !have[want] {
			t.Fatalf("supported-routes missing required template %s %s", want.Method, want.Path)
		}
	}
}

func TestPhase8ProvenanceDocumentsObservedAndCurated(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(phase8Dir, "PROVENANCE.md"))
	if err != nil {
		t.Fatalf("read PROVENANCE.md: %v", err)
	}
	text := string(raw)
	for _, needle := range []string{
		"observed-web-stable.jsonl",
		"supported-routes.json",
		"generated from OpenAPI",
		"login-home",
		"search-detail",
		"1e76e14a9c99507eb9f54361126f22c4658fc1588b2a710a99ba42f2335ff59a",
		"151.0.7922.10",
		"observed-detail-image.tsv",
		"tokenless",
		"HideFromResume",
		"ScheduledTasks",
		"WebSocket",
		"Compatibility-decision media",
		"stream.{Container}",
		"treated as route authorization",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("PROVENANCE.md missing %q", needle)
		}
	}
}

// TestPhase8CorpusRuntimeBidirectionalDrift ensures curated support and the
// declarative Inventory() stay aligned without production JSON parsing.
//
// Direction A: every supported-routes method+template classifies non-Unclassified
// with matching ownership/operation/authMode/projection/egress from Inventory.
// Direction B: every Inventory method+template appears exactly once in corpus.
// Direction C: denied near-misses stay Unclassified and out of corpus.
func TestPhase8CorpusRuntimeBidirectionalDrift(t *testing.T) {
	doc := loadSupportedRoutes(t)
	inv := Inventory()

	type invKey struct {
		Method, Path string
	}
	invByKey := make(map[invKey]InventoryRule)
	var invEntries []invKey
	for _, rule := range inv {
		for _, method := range rule.Methods {
			k := invKey{Method: method, Path: rule.PathTemplate}
			if _, dup := invByKey[k]; dup {
				t.Fatalf("duplicate inventory entry %s %s", method, rule.PathTemplate)
			}
			invByKey[k] = rule
			invEntries = append(invEntries, k)
		}
	}

	// --- A: corpus → inventory + classifier ---
	corpusKeys := make(map[invKey]int)
	for _, route := range doc.Routes {
		k := invKey{Method: route.Method, Path: route.PathTemplate}
		corpusKeys[k]++
		if corpusKeys[k] > 1 {
			t.Fatalf("duplicate corpus entry %s %s", route.Method, route.PathTemplate)
		}
		rule, ok := invByKey[k]
		if !ok {
			t.Fatalf("corpus route not in inventory: %s %s %s", route.ID, route.Method, route.PathTemplate)
		}
		if route.Ownership != OwnershipName(rule.Ownership) {
			t.Fatalf("corpus %s ownership %q != inventory %q", route.ID, route.Ownership, OwnershipName(rule.Ownership))
		}
		if route.Operation != OperationName(rule.Operation) {
			t.Fatalf("corpus %s operation %q != inventory %q", route.ID, route.Operation, OperationName(rule.Operation))
		}
		if route.AuthMode != rule.AuthMode {
			t.Fatalf("corpus %s authMode %q != inventory %q", route.ID, route.AuthMode, rule.AuthMode)
		}
		if route.Projection != rule.Projection {
			t.Fatalf("corpus %s projection %q != inventory %q", route.ID, route.Projection, rule.Projection)
		}
		if route.Egress != rule.Egress {
			t.Fatalf("corpus %s egress %q != inventory %q", route.ID, route.Egress, rule.Egress)
		}
		concrete := instantiatePathTemplate(route.PathTemplate)
		d := Classify(route.Method, concrete)
		if d.Ownership == Unclassified || d.Operation == OperationUnclassified {
			t.Fatalf("corpus route %s classified Unclassified (concrete %q)", route.ID, concrete)
		}
		if !d.MethodAllowed {
			t.Fatalf("corpus route %s method not allowed on %q: %+v", route.ID, concrete, d)
		}
		if d.Ownership != rule.Ownership || d.Operation != rule.Operation {
			t.Fatalf("corpus route %s classify mismatch: got ownership/op %v/%v want %v/%v",
				route.ID, d.Ownership, d.Operation, rule.Ownership, rule.Operation)
		}
	}

	// --- B: inventory → corpus (exhaustive) ---
	for _, k := range invEntries {
		if corpusKeys[k] != 1 {
			t.Fatalf("inventory entry missing or duplicated in corpus: %s %s count=%d", k.Method, k.Path, corpusKeys[k])
		}
		d := Classify(k.Method, instantiatePathTemplate(k.Path))
		if d.Ownership == Unclassified || !d.MethodAllowed {
			t.Fatalf("inventory entry does not classify allowed: %s %s -> %+v", k.Method, k.Path, d)
		}
	}
	if len(doc.Routes) != len(invEntries) {
		t.Fatalf("corpus routes %d != inventory method-entries %d", len(doc.Routes), len(invEntries))
	}

	// --- C: denied near misses ---
	denied := []struct{ method, path string }{
		{"GET", "/Items"},
		{"GET", "/Shows/{Id}/Episodes"},
		{"GET", "/Shows/{Id}/Seasons"},
		{"GET", "/Videos/{ItemId}/original.mkv"},
		{"GET", "/ScheduledTasks"},
		{"POST", "/Users/{UserId}/HideFromResume/{ItemId}"},
		{"PATCH", "/DisplayPreferences/{Id}"},
	}
	for _, dkey := range denied {
		if _, ok := invByKey[invKey{dkey.method, dkey.path}]; ok {
			t.Fatalf("denied near-miss must not be in inventory: %s %s", dkey.method, dkey.path)
		}
		// PATCH DisplayPreferences is known-route method denial, not Unclassified.
		if dkey.method == "PATCH" && dkey.path == "/DisplayPreferences/{Id}" {
			d := Classify(dkey.method, instantiatePathTemplate(dkey.path))
			if d.Ownership != LocalPersonal || d.MethodAllowed {
				t.Fatalf("PATCH DisplayPreferences want LocalPersonal MethodAllowed=false, got %+v", d)
			}
			continue
		}
		if corpusKeys[invKey{dkey.method, dkey.path}] != 0 {
			t.Fatalf("denied near-miss must not be in corpus: %s %s", dkey.method, dkey.path)
		}
		d := Classify(dkey.method, instantiatePathTemplate(dkey.path))
		if d.Ownership != Unclassified {
			t.Fatalf("denied near-miss must classify Unclassified: %s %s got %+v", dkey.method, dkey.path, d)
		}
	}
}

func instantiatePathTemplate(tmpl string) string {
	repl := map[string]string{
		"{UserId}":             "gateway-user",
		"{ItemId}":             "item-1",
		"{Id}":                 "sid-1",
		"{Command}":            "Pause",
		"{MediaSourceId}":      "ms-1",
		"{Index}":              "0",
		"{StartPositionTicks}": "1000",
		"{ImageType}":          "Primary",
		"{Container}":          "mp4",
		"{SegmentId}":          "seg0",
		"{SegmentContainer}":   "ts",
		"{PlaylistId}":         "pl1",
		"{Format}":             "vtt",
	}
	out := tmpl
	// Longer placeholders first so {SegmentContainer} wins over {Container}.
	keys := make([]string, 0, len(repl))
	for k := range repl {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		out = strings.ReplaceAll(out, k, repl[k])
	}
	// stream.{Container} style after replace becomes stream.mp4 already if {Container} replaced.
	return out
}

func ownershipFromCorpus(s string) Ownership {
	switch s {
	case "LocalPublic":
		return LocalPublic
	case "LocalPersonal":
		return LocalPersonal
	case "LocalSession":
		return LocalSession
	case "MetadataProxy":
		return MetadataProxy
	case "MediaProxy":
		return MediaProxy
	case "DeniedSession":
		return DeniedSession
	default:
		return Unclassified
	}
}

func operationMatchesCorpus(got Operation, corpus string) bool {
	// Corpus uses short names; map families rather than requiring 1:1 enum strings.
	switch corpus {
	case "Authenticate":
		return got == OperationAuthenticate
	case "PublicSystemInfo":
		return got == OperationPublicSystemInfo
	case "Ping":
		return got == OperationPing
	case "Logout":
		return got == OperationLogout
	case "PublicUsers":
		return got == OperationPublicUsers
	case "CurrentUser":
		return got == OperationCurrentUser
	case "BrandingConfiguration":
		return got == OperationBrandingConfiguration
	case "BrandingCSS":
		return got == OperationBrandingCSS
	case "Personal":
		return got == OperationPersonal
	case "SessionList":
		return got == OperationSessionList
	case "PlaybackReport":
		return got == OperationPlaybackReport
	case "PlaybackPing":
		return got == OperationPlaybackPing
	case "Capabilities":
		return got == OperationCapabilities
	case "WebSocket":
		return got == OperationWebSocket
	case "SessionPlay":
		return got == OperationSessionPlay
	case "SessionPlaystate":
		return got == OperationSessionPlaystate
	case "SessionGeneralCommand":
		return got == OperationSessionGeneralCommand
	case "MetadataProxy":
		return got == OperationMetadataProxy
	case "MediaProxy":
		return got == OperationMediaProxy
	case "PlaybackInfo":
		return got == OperationPlaybackInfo
	case "LiveStreamOpen":
		return got == OperationLiveStreamOpen
	case "LiveStreamMediaInfo":
		return got == OperationLiveStreamMediaInfo
	case "LiveStreamClose":
		return got == OperationLiveStreamClose
	case "ActiveEncodingsDelete":
		return got == OperationActiveEncodingsDelete
	case "ActiveEncodingsDeleteCompat":
		return got == OperationActiveEncodingsDeleteCompat
	default:
		return false
	}
}
