//go:build integration

package itest

import (
	"github.com/brickKit/be-sdk-go/v3/ids"
	assert "github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

// warehouses: a code is taken once in a legal entity; a legal entity mdm/org does not know is refused.
func (fx *fixture) warehouses() {
	t := fx.t
	// warehouses
	code, sh := fx.alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": fx.le, "code": "SH-01", "name": "上海一号仓"})
	require.Equal(t, 200, code, sh)
	_, bj := fx.alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": fx.le, "code": "BJ-01", "name": "北京一号仓"})
	_, hk := fx.alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": fx.otherLE, "code": "HK-01", "name": "香港仓"})
	fx.whSH, fx.whBJ, fx.whHK, fx.sh = sh["id"].(string), bj["id"].(string), hk["id"].(string), sh
	code, dup := fx.alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": fx.le, "code": "SH-01", "name": "又一个"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "CODE_TAKEN", dup["reason"])
	code, bad := fx.alice.do("POST", "/api/inventory/warehouses", map[string]any{"legal_entity_id": ids.New().String(), "code": "X", "name": "x"})
	assert.Equal(t, 409, code)
	assert.Equal(t, "LEGAL_ENTITY_NOT_USABLE", bad["reason"])
}
