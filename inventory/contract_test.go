package inventory

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/brickKit/be-sdk-go/v3/httpx"
	"github.com/brickKit/be-sdk-go/v3/launcher"
	"github.com/brickKit/be-sdk-go/v3/mergesafety"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheManifestDeclaresWhatTheSDKReads: component.yaml's configSchema is exactly the keys the SDK reads.
func TestTheManifestDeclaresWhatTheSDKReads(t *testing.T) {
	raw, err := os.ReadFile("../component.yaml")
	require.NoError(t, err)
	if p := launcher.ManifestProblems(raw, Definition(), nil); len(p) > 0 {
		t.Errorf("component.yaml's configSchema differs from what the SDK reads:\n%v\nthe block:\n%s", p,
			launcher.ConfigSchemaYAML(Definition(), nil))
	}
}

// TestTheManifestListsThePublishedEvents: component.yaml's events.publishes is exactly the event contracts.
func TestTheManifestListsThePublishedEvents(t *testing.T) {
	raw, err := os.ReadFile("../component.yaml")
	require.NoError(t, err)
	assert.Empty(t, launcher.EventProblems(raw, os.DirFS("../contract/events")))
}

// TestTheRoutesAreTheOpenAPIDocument: every route is in contract/openapi.yaml with its operation, and nothing else is.
func TestTheRoutesAreTheOpenAPIDocument(t *testing.T) {
	raw, err := os.ReadFile("../contract/openapi.yaml")
	require.NoError(t, err)
	var r httpx.Routes
	require.NoError(t, (&Module{}).Routes(&r))
	assert.Empty(t, httpx.OpenAPIProblems(raw, r.List()))
}

// TestThePayloadsFitTheirSchemas: what events and reads carry is what contract/events promises.
func TestThePayloadsFitTheirSchemas(t *testing.T) {
	le, wh, pr := "01a15ab0-0000-7000-8000-0000000000a1", "01a15ab0-0000-7000-8000-0000000000b1", "01a15ab0-0000-7000-8000-0000000000c1"
	batch, line, branch, rev := "B2026-10", "01a15ab0-0000-7000-8000-0000000000e1", "01a15ab0-0000-7000-8000-0000000000f1", int64(2)
	at := "2026-10-20T08:00:00.000000Z"
	reason := "insufficient_stock"
	for typ, v := range map[string]any{
		WarehouseChanged: Warehouse{ID: wh, LegalEntityID: le, Code: "SH-01", Name: "上海一号仓", State: "active", Version: 1, UpdatedAt: at},
		PositionChanged: Position{ID: "01a15ab0-0000-7000-8000-0000000000d1", ProductID: pr, WarehouseID: wh, LegalEntityID: le,
			OnHand: "12.5", Reserved: "2", Available: "10.5", Version: 3, UpdatedAt: at},
		MovementRecorded: Movement{FactID: "01a15ab0-0000-7000-8000-0000000000d2", Kind: "issue", ProductID: pr, WarehouseID: wh,
			LegalEntityID: le, Quantity: "2", Batch: &batch, Serials: []string{}, BusinessDate: "2026-10-20",
			Reference: &Reference{Kind: "sales_order", ID: "SO-1", LineID: &line}, BranchID: &branch, ConsumptionID: &line,
			InventorySeq: &Seq{Epoch: "1.1", Seq: 7}, ValuationSeq: &Seq{Epoch: "1.1", Seq: 3}, RecordedAt: at},
		BranchChanged: Branch{ID: branch, LegalEntityID: &le, Reference: &Reference{Kind: "sales_order", ID: "SO-1", Revision: &rev},
			State: "rejected", Reason: &reason, Lines: []BranchLine{{ProductID: pr, WarehouseID: wh, Quantity: "3", Consumed: "0"}},
			Version: 1, UpdatedAt: at},
	} {
		c := jsonschema.NewCompiler()
		s, err := c.Compile("../contract/events/" + typ + ".schema.json")
		require.NoError(t, err, typ)
		b, err := json.Marshal(v)
		require.NoError(t, err)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		require.NoError(t, err)
		assert.NoError(t, s.Validate(doc), typ)
	}
	// a tombstone: a cancel before the hold
	tomb, _ := json.Marshal(Branch{ID: branch, State: "cancelled", Lines: []BranchLine{}, Version: 1, UpdatedAt: at})
	c := jsonschema.NewCompiler()
	s, err := c.Compile("../contract/events/" + BranchChanged + ".schema.json")
	require.NoError(t, err)
	doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(tomb))
	assert.NoError(t, s.Validate(doc), "tombstone")
}

// TestTheModuleIsMergeSafe: the module is compiled into the Go core shell.
func TestTheModuleIsMergeSafe(t *testing.T) {
	findings, err := mergesafety.CheckDir(".")
	require.NoError(t, err)
	for _, f := range findings {
		t.Errorf("%v", f)
	}
}
