package module

import (
	"testing"

	besdk "github.com/brickKit/be-sdk-go"
)

// TestLowStockThreshold_读LOW_STOCK_THRESHOLD：模块按 configSchema 里的确切键名
// 读低库存阈值——配了非默认值就用它，没配（或空串）用默认值 10，配了非法值
// 让模块启动失败，而不是悄悄退回默认值。
func TestLowStockThreshold_读LOW_STOCK_THRESHOLD(t *testing.T) {
	for _, c := range []struct {
		values map[string]string
		want   string
	}{
		{map[string]string{"LOW_STOCK_THRESHOLD": "25"}, "25"},
		{map[string]string{"LOW_STOCK_THRESHOLD": "2.5"}, "2.5"},
		{map[string]string{}, "10"},
		{map[string]string{"LOW_STOCK_THRESHOLD": ""}, "10"},
		{map[string]string{"lowStockThreshold": "25"}, "10"}, // 驼峰旧写法不是这个键
	} {
		got, err := lowStockThreshold(besdk.NewConfig(c.values))
		if err != nil {
			t.Fatalf("%v：不该报错：%v", c.values, err)
		}
		if got != c.want {
			t.Fatalf("%v：阈值应为 %q，实际 %q", c.values, c.want, got)
		}
	}
	for _, bad := range []string{"abc", "-1", "1e3"} {
		if _, err := lowStockThreshold(besdk.NewConfig(map[string]string{"LOW_STOCK_THRESHOLD": bad})); err == nil {
			t.Fatalf("LOW_STOCK_THRESHOLD=%q 不合法，模块应拒绝启动", bad)
		}
	}
}
