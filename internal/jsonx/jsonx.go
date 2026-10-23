// Package jsonx 는 Tumblebug · Beetle 응답처럼 모양이 정해지지 않은 JSON 을 다루는 도구다.
//
// 숫자는 json.Number 로 읽는다. Beetle 후보를 고쳐 다시 보낼 때 숫자 표기가 바뀌지 않고,
// 구조체에 없는 필드가 빠지지 않게 하려는 것이다.
package jsonx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// M 은 JSON 객체다.
type M = map[string]any

// Decode 는 숫자를 json.Number 로 남겨 읽는다.
func Decode(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// DecodeInto 는 구조체 등으로 읽는다 (숫자는 json.Number).
func DecodeInto(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

// RoundTrip 은 Go 값을 JSON 으로 썼다가 다시 읽는다 — 상태 파일에 넣은 값과 다시 읽은 값의 타입을 같게 맞춘다.
func RoundTrip(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	out, err := Decode(b)
	if err != nil {
		return v
	}
	return out
}

// Marshal 은 HTML 이스케이프 없이 쓴다 (<, >, & 를 그대로).
func Marshal(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// WriteFile 은 들여쓴 JSON 을 파일로 쓴다.
func WriteFile(path string, v any) error {
	b, err := Marshal(v, true)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadFile 은 파일을 읽어 any 로.
func ReadFile(path string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

// AsM 은 객체면 그대로, 아니면 nil.
func AsM(v any) M {
	if m, ok := v.(M); ok {
		return m
	}
	return nil
}

// AsList 는 배열이면 그대로, 아니면 nil.
func AsList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []M:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = m
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	return nil
}

// ListM 은 배열 안의 객체만 모은다.
func ListM(v any) []M {
	var out []M
	for _, x := range AsList(v) {
		if m := AsM(x); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// Strings 는 배열 안의 값을 문자열로.
func Strings(v any) []string {
	var out []string
	for _, x := range AsList(v) {
		out = append(out, Str(x))
	}
	return out
}

// Str 은 값을 파이썬 str() 처럼 문자열로 바꾼다. nil 은 "".
func Str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatFloat(t, 'f', 1, 64)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

// Float 은 숫자 · 숫자 문자열을 float64 로 (아니면 ok=false).
func Float(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// F 는 Float 의 값만 (실패하면 0).
func F(v any) float64 {
	f, _ := Float(v)
	return f
}

// Int 는 정수로 (소수는 버림, 실패하면 0).
func Int(v any) int {
	f, ok := Float(v)
	if !ok {
		return 0
	}
	return int(f)
}

// IsInt 는 JSON 정수인지 (문자열 · 소수는 아니다).
func IsInt(v any) bool {
	switch t := v.(type) {
	case int, int64, int32:
		return true
	case json.Number:
		_, err := t.Int64()
		return err == nil
	}
	return false
}

// Truthy 는 파이썬의 참 거짓 판단을 따른다 (0 · "" · nil · 빈 배열 · 빈 객체는 거짓).
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case M:
		return len(t) > 0
	case []M:
		return len(t) > 0
	case []string:
		return len(t) > 0
	}
	return true
}

// Or 는 파이썬의 `a or b`.
func Or(a, b any) any {
	if Truthy(a) {
		return a
	}
	return b
}

// S 는 객체의 키를 문자열로.
func S(m M, key string) string {
	if m == nil {
		return ""
	}
	return Str(m[key])
}

// SOr 는 키 값이 비면 기본값.
func SOr(m M, key, def string) string {
	if s := S(m, key); s != "" {
		return s
	}
	return def
}

// Get 은 점으로 이은 경로를 따라 내려간다 (없으면 nil).
func Get(v any, path ...string) any {
	cur := v
	for _, p := range path {
		m := AsM(cur)
		if m == nil {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// Clone 은 JSON 값을 깊이 복사한다.
func Clone(v any) any {
	switch t := v.(type) {
	case M:
		out := make(M, len(t))
		for k, x := range t {
			out[k] = Clone(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Clone(x)
		}
		return out
	}
	return v
}

// SortedKeys 는 키를 정렬해 돌려준다.
func SortedKeys(m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Equal 은 두 JSON 값이 같은지 (숫자는 값으로 비교).
func Equal(a, b any) bool {
	ab, err1 := Marshal(normalize(a), false)
	bb, err2 := Marshal(normalize(b), false)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

func normalize(v any) any {
	switch t := v.(type) {
	case M:
		out := make(M, len(t))
		for k, x := range t {
			out[k] = normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalize(x)
		}
		return out
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return v
}
