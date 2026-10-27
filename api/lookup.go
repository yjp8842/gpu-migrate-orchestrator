package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gpu-migrate-orchestrator/internal/beetle"
	"gpu-migrate-orchestrator/internal/jsonx"
	"gpu-migrate-orchestrator/internal/logx"
	"gpu-migrate-orchestrator/internal/tumblebug"
)

// Tumblebug · Beetle 직접 호출 — 화면의 선택지(소스 · 리전) 조회와 타겟 삭제에만 쓴다. 나머지는 전부 migrate 가 한다.
// 인프라 목록 GET 은 출발 IP 당 2 req/s 라(MapUI 와 공유) 짧게 캐시하고, 클라이언트 하나를 같이 써 호출 간격을 지킨다.

type cache struct {
	mu sync.Mutex
	m  map[string]cached
}

type cached struct {
	at    time.Time
	value any
}

func (c *cache) get(key string, ttl time.Duration, fn func() (any, error)) (any, error) {
	c.mu.Lock()
	if hit, ok := c.m[key]; ok && time.Since(hit.at) < ttl {
		c.mu.Unlock()
		return hit.value, nil
	}
	c.mu.Unlock()
	v, err := fn()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.m[key] = cached{at: time.Now(), value: v}
	c.mu.Unlock()
	return v, nil
}

func (s *Server) tumblebug() (*tumblebug.Client, error) {
	s.tbOnce.Do(func() {
		st, err := s.settings()
		if err != nil {
			s.tbErr = err
			return
		}
		ep, err := st.Tumblebug("default")
		if err != nil {
			s.tbErr = err
			return
		}
		s.tb = tumblebug.New(ep, logx.Quiet(), st.Pacing, false)
	})
	return s.tb, s.tbErr
}

// summarizeInfra 는 소스 후보 — Tumblebug 인프라를 화면용으로 줄인다 (첫 노드 기준).
func summarizeInfra(infra M) M {
	n := M{}
	if l := jsonx.ListM(infra["node"]); len(l) > 0 {
		n = l[0]
	}
	spec, image, region := jsonx.AsM(n["spec"]), jsonx.AsM(n["image"]), jsonx.AsM(n["region"])
	gpu := ""
	if model := jsonx.S(spec, "acceleratorModel"); model != "" {
		gpu = model
		if c := jsonx.Int(spec["acceleratorCount"]); c > 1 {
			gpu += fmt.Sprintf(" × %d", c)
		}
		if g := jsonx.F(spec["acceleratorMemoryGB"]); g > 0 {
			gpu += fmt.Sprintf(" · %gGB", g)
		}
	}
	specName := jsonx.S(n, "cspSpecName")
	if specName == "" {
		id := jsonx.S(n, "specId")
		specName = id[strings.LastIndex(id, "+")+1:]
	}
	madeBy := ""
	switch {
	case jsonx.S(jsonx.AsM(infra["label"]), "gmp.case") != "":
		madeBy = "migrate"
	case strings.Contains(jsonx.S(infra, "description"), "mapui"):
		madeBy = "MapUI"
	}
	status, _, _ := strings.Cut(jsonx.S(infra, "status"), ":")
	csp, _, _ := strings.Cut(jsonx.S(n, "connectionName"), "-")
	return M{"id": jsonx.S(infra, "id"), "status": status, "csp": csp, "region": jsonx.S(region, "region"), "zone": jsonx.S(region, "zone"),
		"spec": specName, "vcpu": spec["vCPU"], "memoryGiB": spec["memoryGiB"], "gpu": gpu, "os": jsonx.S(image, "osType"),
		"publicIP": jsonx.S(n, "publicIP"), "madeBy": madeBy}
}

// listNamespaces 는 Tumblebug 네임스페이스 목록 — default 를 맨 앞에, system(Tumblebug 공용 사양 · 이미지 자리)은 뺀다.
func (s *Server) listNamespaces() (any, error) {
	return s.cache.get("ns", time.Minute, func() (any, error) {
		tb, err := s.tumblebug()
		if err != nil {
			return nil, err
		}
		v, err := tb.Req("GET", "/ns", tumblebug.Opts{})
		if err != nil {
			return nil, err
		}
		return namespaces(jsonx.ListM(jsonx.AsM(v)["ns"])), nil
	})
}

func namespaces(list []M) []M {
	out := []M{}
	for _, n := range list {
		if id := jsonx.S(n, "id"); nsRe.MatchString(id) && id != "system" {
			out = append(out, M{"id": id, "description": jsonx.S(n, "description")})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i]["id"].(string), out[j]["id"].(string)
		if (a == "default") != (b == "default") {
			return a == "default"
		}
		return a < b
	})
	return out
}

func (s *Server) listSources(ns string) (any, error) {
	if !nsRe.MatchString(ns) {
		return nil, fmt.Errorf("네임스페이스 이름이 이상합니다: %q", ns)
	}
	return s.cache.get("infra:"+ns, 10*time.Second, func() (any, error) {
		tb, err := s.tumblebug()
		if err != nil {
			return nil, err
		}
		v, err := tb.Req("GET", "/ns/"+ns+"/infra", tumblebug.Opts{Route: "infra_get"})
		if err != nil {
			return nil, err
		}
		out := []M{}
		for _, in := range jsonx.ListM(jsonx.AsM(v)["infra"]) {
			if x := summarizeInfra(in); x["status"] != "Terminated" {
				out = append(out, x)
			}
		}
		return out, nil
	})
}

// groupRegions 는 Tumblebug 연결 목록을 CSP 별 리전 · 존 · 위치 이름으로 묶는다.
func groupRegions(conns []M) map[string][]M {
	out := map[string][]M{}
	for _, c := range conns {
		csp, rd := jsonx.S(c, "providerName"), jsonx.AsM(c["regionDetail"])
		region := jsonx.S(rd, "regionId")
		if csp == "" || region == "" {
			continue
		}
		lbl := jsonx.S(jsonx.AsM(rd["location"]), "display")
		if lbl == "" {
			lbl = jsonx.SOr(rd, "description", region)
		}
		zones := jsonx.Strings(rd["zones"])
		if zones == nil {
			zones = []string{}
		}
		out[csp] = append(out[csp], M{"connection": jsonx.S(c, "configName"), "region": region, "label": lbl, "zones": zones,
			"defaultZone": jsonx.S(jsonx.AsM(c["regionZoneInfo"]), "assignedZone")})
	}
	for k := range out {
		sort.Slice(out[k], func(i, j int) bool { return out[k][i]["region"].(string) < out[k][j]["region"].(string) })
	}
	return out
}

func (s *Server) listRegions() (any, error) {
	return s.cache.get("conn", 10*time.Minute, func() (any, error) {
		tb, err := s.tumblebug()
		if err != nil {
			return nil, err
		}
		v, err := tb.Req("GET", "/connConfig", tumblebug.Opts{})
		if err != nil {
			return nil, err
		}
		d := jsonx.AsM(v)
		conns := jsonx.ListM(d["connectionconfig"])
		if conns == nil {
			conns = jsonx.ListM(d["connectionConfig"])
		}
		return groupRegions(conns), nil
	})
}

// infraStatus 는 Tumblebug 상태 문자열의 앞부분이다 ('Suspended:1 (S:1/1)' → Suspended). 인프라가 없으면 NotFound.
func infraStatus(infra M) string {
	if infra == nil {
		return "NotFound"
	}
	st, _, _ := strings.Cut(jsonx.S(infra, "status"), ":")
	return or(st, "Unknown")
}

// sourceStatus 는 소스 인프라의 지금 상태다 — Migrate 직전 확인용이라 캐시하지 않는다.
func (s *Server) sourceStatus(ns, id string) (string, error) {
	if !nsRe.MatchString(ns) || !idRe.MatchString(id) {
		return "", fmt.Errorf("소스 이름이 이상합니다")
	}
	tb, err := s.tumblebug()
	if err != nil {
		return "", err
	}
	infra, err := tb.GetInfra(ns, id)
	if err != nil {
		return "", err
	}
	return infraStatus(infra), nil
}

// deleteTarget 은 Beetle 로 만든 타겟을 지운다 — vNet · 보안 그룹 · SSH 키까지 (option=terminate).
func (s *Server) deleteTarget(ns, id string) (any, error) {
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	if st.Beetle == nil {
		return nil, errors.New("settings 에 beetle 이 없습니다")
	}
	return beetle.New(*st.Beetle, logx.Quiet(), false).DeleteInfra(ns, id)
}
