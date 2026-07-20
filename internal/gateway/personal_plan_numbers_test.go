package gateway

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const oracleBigInteger = "9223372036854775808123"

func TestDecodeJSONUseNumberRequiresOneValueAndPreservesIntegers(t *testing.T) {
	var value map[string]any
	if err := decodeJSONUseNumber([]byte(`{"Big":`+oracleBigInteger+`}`), &value); err != nil {
		t.Fatal(err)
	}
	if got, ok := value["Big"].(json.Number); !ok || got.String() != oracleBigInteger {
		t.Fatalf("Big = %#v", value["Big"])
	}
	for _, data := range []string{"", `{"a":1} {"b":2}`, `{"a":1} trailing`} {
		if err := decodeJSONUseNumber([]byte(data), &value); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}

func TestBoundedNonNegativeJSONInt(t *testing.T) {
	maxInt := strconv.FormatUint(uint64(^uint(0)>>1), 10)
	tests := []struct {
		name  string
		value any
		want  int
		ok    bool
	}{
		{"number", json.Number("42"), 42, true},
		{"exponent", json.Number("4e1"), 40, true},
		{"float", float64(7), 7, true},
		{"max", json.Number(maxInt), int(^uint(0) >> 1), true},
		{"negative", json.Number("-1"), 0, false},
		{"fraction", json.Number("1.5"), 0, false},
		{"too large", json.Number(oracleBigInteger), 0, false},
		{"string", "1", 0, false},
		{"nan number", json.Number("NaN"), 0, false},
		{"inf number", json.Number("Inf"), 0, false},
		{"nan float", math.NaN(), 0, false},
		{"inf float", math.Inf(1), 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := boundedNonNegativeJSONInt(test.value)
			if got != test.want || ok != test.ok {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestJSONNumberPlannerSortAndPaging(t *testing.T) {
	items := []resolvedPersonalItem{
		{item: map[string]any{"Id": "high", "Rank": json.Number("9007199254740993")}},
		{item: map[string]any{"Id": "low", "Rank": json.Number("9007199254740992")}},
	}
	sortPersonalPlanItems(items, []personalSortTerm{{Name: "Rank", Source: personalSortMetadata}})
	if got := sortedIDs(items); !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Fatalf("order = %v", got)
	}
	page, start, total, err := strictPersonalPage(map[string]any{
		"Items": []any{}, "StartIndex": json.Number("2"), "TotalRecordCount": json.Number("9e0"),
	}, personalShapeQueryResult, 2)
	if err != nil || len(page) != 0 || start == nil || *start != 2 || total == nil || *total != 9 {
		t.Fatalf("page=%v start=%v total=%v err=%v", page, start, total, err)
	}
	for _, invalid := range []json.Number{"-1", "1.5", oracleBigInteger, "NaN", "Inf"} {
		if _, err := optionalJSONInt(map[string]any{"Value": invalid}, "Value"); err == nil {
			t.Fatalf("accepted paging number %q", invalid)
		}
		item := map[string]any{"Value": invalid}
		if _, ok := nextUpNonnegativeInt(item, "Value"); ok {
			t.Fatalf("accepted NextUp number %q", invalid)
		}
	}
}

func TestOracleBigIntegerSurvivesPlannerStages(t *testing.T) {
	assertBig := func(t *testing.T, item map[string]any) {
		t.Helper()
		got, ok := item["Big"].(json.Number)
		if !ok || got.String() != oracleBigInteger {
			t.Fatalf("Big = %#v", item["Big"])
		}
	}

	t.Run("candidate", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"item","Big":` + oracleBigInteger + `}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
		source, _ := newPersonalPlanSourceTestSource(t, fake)
		page, err := source.fetchCandidatePage(context.Background(), personalPlan{Kind: personalPlanNegative, Shape: personalShapeQueryResult, Path: "/Items"}, 0, 10)
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("page=%#v err=%v", page, err)
		}
		assertBig(t, page.Items[0])
	})

	t.Run("resolution", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"item","Big":` + oracleBigInteger + `}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
		source, _ := newPersonalPlanSourceTestSource(t, fake)
		resolved, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanNegative}, personalPlanSourceSnapshot(nil), []string{"item"})
		if err != nil || len(resolved) != 1 {
			t.Fatalf("resolved=%#v err=%v", resolved, err)
		}
		assertBig(t, resolved[0].item)
	})

	t.Run("structural refinement", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"item","Big":` + oracleBigInteger + `}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
		source, _ := newPersonalPlanSourceTestSource(t, fake)
		refined, err := source.refineResolved(context.Background(), personalPlan{Refinement: url.Values{"ParentId": {"parent"}}}, personalPlanSourceResolvedItems([]string{"item"}))
		if err != nil || len(refined) != 1 {
			t.Fatalf("refined=%#v err=%v", refined, err)
		}
		assertBig(t, refined[0].item)
	})

	t.Run("projection", func(t *testing.T) {
		server := &Server{}
		request := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items", nil)
		item := server.rewritePlannedPersonalItem(map[string]any{"Id": "item", "Big": json.Number(oracleBigInteger)}, &Session{SyntheticUserID: "synthetic-user"}, upstreamRequestSnapshot{}, "gateway-token", request)
		assertBig(t, item)
		projected, err := server.projectPlannedPersonalItems([]resolvedPersonalItem{{item: item, state: PlaybackState{ItemID: "item"}}}, &Session{SyntheticUserID: "synthetic-user"})
		if err != nil || len(projected) != 1 {
			t.Fatalf("projected=%#v err=%v", projected, err)
		}
		assertBig(t, projected[0])
	})
}

func TestPersonalPlanHTTPLocalOutputPreservesOracleBigInteger(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"item","Big":` + oracleBigInteger + `}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
	server, store, _ := personalPlanHTTPServer(t, fake)
	defer server.Close()
	if err := store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item", IsFavorite: true}); err != nil {
		t.Fatal(err)
	}
	response := personalPlanHTTPRequest(server, "/Items?api_key=gateway-token&IsFavorite=true")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"Big":`+oracleBigInteger) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
