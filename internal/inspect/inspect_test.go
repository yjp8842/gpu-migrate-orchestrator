package inspect

import (
	"testing"

	"gpu-migrate-orchestrator/internal/config"
	"gpu-migrate-orchestrator/internal/jsonx"
)

func TestLLMMismatch(t *testing.T) {
	if bad, _ := LLMMismatch(jsonx.M{"llm_temperature": 0.0, "llm_seed": 42}, "0", "42"); bad {
		t.Fatal("같은데 다르다고 했다")
	}
	if bad, _ := LLMMismatch(jsonx.M{"llm_temperature": 0.7, "llm_seed": nil}, "0", "42"); !bad {
		t.Fatal("다른데 같다고 했다")
	}
	if bad, _ := LLMMismatch(jsonx.M{}, "0", "42"); !bad {
		t.Fatal("값이 없는데 같다고 했다")
	}
	if bad, _ := LLMMismatch(jsonx.M{}, "", ""); bad {
		t.Fatal("사례에 기준이 없는데 다르다고 했다")
	}
}

func TestIdentifyParsesOutputs(t *testing.T) {
	ad := &config.Adapter{Raw: jsonx.M{"form": "shell", "workload": "widgetrag", "services": []any{"ollama"},
		"identify": jsonx.M{"units": []any{"widgetrag-ai"}, "env_file": "/etc/x.env"}}}
	if Commands(ad)[0].Name != "gpu" {
		t.Fatal("순서")
	}
	id := Identify(ad, map[string]string{"gpu": "0, GPU-1, Tesla T4, 595.45.04, 00000000:00:1E.0, 15360", "cuda": "CUDA Version: 13.0",
		"os": `PRETTY_NAME="Ubuntu 24.04.3 LTS"`, "units": "Id=widgetrag-ai.service\nActiveState=active\n---",
		"env_keys": "LLM_SEED=\nLLM_TEMPERATURE=", "code_ref": "7fbeb17\n", "health": `{"llm_temperature":0,"llm_seed":42}`})
	g := jsonx.ListM(id["gpu"])[0]
	if g["model"] != "Tesla T4" || g["cudaVersion"] != "13.0" || g["memoryTotalGB"] != 15 {
		t.Fatal(g)
	}
	if jsonx.ListM(id["units"])[0]["activeState"] != "active" || jsonx.Strings(id["env_keys"])[1] != "LLM_TEMPERATURE" {
		t.Fatal(id)
	}
	if jsonx.S(jsonx.AsM(id["health"]), "llm_seed") != "42" || id["code_ref"] != "7fbeb17" || id["os"] != "Ubuntu 24.04.3 LTS" {
		t.Fatal(id)
	}
}
