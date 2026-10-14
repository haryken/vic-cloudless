package xiaozhi

import "testing"

func TestSameDirectRouteSkipsSettingsOnlySave(t *testing.T) {
	cfg := Config{Enabled: true, Backend: BackendOpenAI, IdentityMode: "vi_pool", DeviceID: "aa"}
	rt := reloadRoute(cfg, OpenAIModeRealtime)

	if !sameDirectRoute(rt, reloadRoute(cfg, OpenAIModeRealtime), cfg) {
		t.Fatal("saving OpenAI settings in the same mode should not interrupt")
	}
	if sameDirectRoute(rt, reloadRoute(cfg, OpenAIModePipeline), cfg) {
		t.Fatal("realtime -> pipeline must reload")
	}
	gem := cfg
	gem.Backend = BackendGemini
	if sameDirectRoute(rt, reloadRoute(gem, OpenAIModeRealtime), gem) {
		t.Fatal("openai -> gemini must reload")
	}
	if sameDirectRoute("", rt, cfg) {
		t.Fatal("unknown previous route must reload")
	}
}

func TestSameDirectRouteXiaozhiAlwaysReloads(t *testing.T) {
	cfg := Config{Enabled: true, Backend: BackendXiaozhi, IdentityMode: "vi_pool", DeviceID: "aa"}
	rt := reloadRoute(cfg, "")
	if sameDirectRoute(rt, rt, cfg) {
		t.Fatal("Xiaozhi reload must reconnect even when the route looks unchanged")
	}
}

func TestReloadRouteIgnoresOpenAIModeForOtherBackends(t *testing.T) {
	cfg := Config{Enabled: true, Backend: BackendGemini}
	if reloadRoute(cfg, OpenAIModeRealtime) != reloadRoute(cfg, OpenAIModePipeline) {
		t.Fatal("OpenAI mode should not matter for Gemini")
	}
}
