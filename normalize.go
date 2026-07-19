package batchbru

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
)

// readOnlyFields are members of types.JobDefinition that are assigned by the
// API and have no counterpart in RegisterJobDefinitionInput.
var readOnlyFields = []string{
	"jobDefinitionArn",
	"revision",
	"status",
	"containerOrchestrationType",
}

// nonFalsyDefaults holds API defaults that are not empty/false, keyed by the
// dotted path from the root of the job definition. Array indexes are elided,
// so containerProperties.secrets[0].name is written as
// containerProperties.secrets.name.
var nonFalsyDefaults = map[string]any{
	"platformCapabilities": []any{"EC2"},
}

// Normalize converts an arbitrary job definition document into the canonical
// form used for diffing and registration. The document is decoded into the SDK
// input type (rejecting unknown members, which catches typos) and re-encoded
// with the member names the Batch API uses.
func Normalize(def map[string]any) (map[string]any, error) {
	in, err := decodeRegisterInput(def)
	if err != nil {
		return nil, err
	}
	return toAPIMap(in), nil
}

// decodeRegisterInput decodes a job definition document into the SDK input type.
func decodeRegisterInput(def map[string]any) (*batch.RegisterJobDefinitionInput, error) {
	b, err := json.Marshal(def)
	if err != nil {
		return nil, fmt.Errorf("failed to encode job definition: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var in batch.RegisterJobDefinitionInput
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("failed to decode job definition: %w", err)
	}
	return &in, nil
}

// RemoteToMap converts a job definition read back from the API into the same
// canonical form as a local definition, so that the two can be compared.
func RemoteToMap(jd types.JobDefinition) (map[string]any, error) {
	m := toAPIMap(jd)
	for _, k := range readOnlyFields {
		delete(m, k)
	}
	return Normalize(m)
}

// toAPIMap encodes an SDK struct using the member names of the Batch API.
//
// The SDK types carry no JSON tags -- the wire format is produced by generated
// serializers -- so encoding/json would emit Go field names. Walking the struct
// and lower-casing the first letter of each field reproduces the API names,
// while map contents (tags, parameters, Kubernetes labels) are passed through
// verbatim.
func toAPIMap(v any) map[string]any {
	out, ok := encodeValue(reflect.ValueOf(v))
	if !ok {
		return map[string]any{}
	}
	m, ok := out.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

var timeType = reflect.TypeOf(time.Time{})

// encodeValue reports the JSON representation of v, and whether v is set at
// all. Unset members (nil pointers, nil slices and maps, empty enum strings)
// are reported as absent so that they do not appear in the encoded document.
func encodeValue(v reflect.Value) (any, bool) {
	if !v.IsValid() {
		return nil, false
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil, false
		}
		return encodeValue(v.Elem())
	case reflect.Struct:
		if v.Type() == timeType {
			return v.Interface().(time.Time).Format(time.RFC3339), true
		}
		m := map[string]any{}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || f.Name == "noSmithyDocumentSerde" {
				continue
			}
			fv, ok := encodeValue(v.Field(i))
			if !ok {
				continue
			}
			m[lowerFirst(f.Name)] = fv
		}
		return m, true
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return nil, false
		}
		s := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			s[i] = encodeElem(v.Index(i))
		}
		return s, true
	case reflect.Map:
		if v.IsNil() {
			return nil, false
		}
		m := map[string]any{}
		iter := v.MapRange()
		for iter.Next() {
			m[fmt.Sprint(iter.Key().Interface())] = encodeElem(iter.Value())
		}
		return m, true
	case reflect.String:
		s := v.String()
		return s, s != ""
	case reflect.Bool:
		return v.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint(), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	default:
		return nil, false
	}
}

// encodeElem encodes a list or map element. Unlike a struct member, an element
// that holds a zero value is still part of the document -- an empty string in
// the parameters map is a value, not an omission -- so the zero value is
// emitted rather than dropped.
func encodeElem(v reflect.Value) any {
	if ev, ok := encodeValue(v); ok {
		return ev
	}
	switch v.Kind() {
	case reflect.String:
		return ""
	case reflect.Bool:
		return false
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int64(0)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return uint64(0)
	case reflect.Float32, reflect.Float64:
		return float64(0)
	default:
		return nil
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[size:]
}

// PruneDefaults removes members from remote that the API filled in with a
// default value and that local does not mention, so that a definition which
// omits optional members does not show a diff against every default the API
// materialised. Members that local does mention are always kept, even when
// they hold a default value.
func PruneDefaults(remote, local map[string]any) map[string]any {
	return pruneObject(remote, local, "").(map[string]any)
}

func pruneObject(remote map[string]any, local map[string]any, path string) any {
	out := map[string]any{}
	for k, rv := range remote {
		p := k
		if path != "" {
			p = path + "." + k
		}
		lv, present := local[k]
		if !present {
			if isDefaultValue(rv, p) {
				continue
			}
			out[k] = rv
			continue
		}
		out[k] = pruneValue(rv, lv, p)
	}
	return out
}

func pruneValue(rv, lv any, path string) any {
	switch r := rv.(type) {
	case map[string]any:
		l, ok := lv.(map[string]any)
		if !ok {
			return rv
		}
		return pruneObject(r, l, path)
	case []any:
		l, ok := lv.([]any)
		if !ok {
			return rv
		}
		out := make([]any, len(r))
		for i, e := range r {
			if i < len(l) {
				out[i] = pruneValue(e, l[i], path)
			} else {
				out[i] = e
			}
		}
		return out
	default:
		return rv
	}
}

// isDefaultValue reports whether v is what the API stores for an unspecified
// member: an empty or false value, or one of the documented non-empty defaults.
func isDefaultValue(v any, path string) bool {
	if def, ok := nonFalsyDefaults[path]; ok && reflect.DeepEqual(v, def) {
		return true
	}
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case float64:
		return t == 0
	case int64:
		return t == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		for k, e := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if !isDefaultValue(e, p) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// RemoteOnlyPaths lists the dotted paths of members that survived PruneDefaults
// on the remote side but do not exist in the local definition. Array indexes
// are elided, the same convention as nonFalsyDefaults. Such a member is either
// something the user removed on purpose, or a default the API materialised that
// nonFalsyDefaults does not know about yet -- a false diff.
func RemoteOnlyPaths(remote, local map[string]any) []string {
	paths := remoteOnlyObject(remote, local, "")
	slices.Sort(paths)
	return slices.Compact(paths)
}

func remoteOnlyObject(remote, local map[string]any, path string) []string {
	var out []string
	for k, rv := range remote {
		p := k
		if path != "" {
			p = path + "." + k
		}
		lv, present := local[k]
		if !present {
			out = append(out, p)
			continue
		}
		out = append(out, remoteOnlyValue(rv, lv, p)...)
	}
	return out
}

func remoteOnlyValue(rv, lv any, path string) []string {
	switch r := rv.(type) {
	case map[string]any:
		l, ok := lv.(map[string]any)
		if !ok {
			return nil
		}
		return remoteOnlyObject(r, l, path)
	case []any:
		l, ok := lv.([]any)
		if !ok {
			return nil
		}
		var out []string
		for i, e := range r {
			if i < len(l) {
				out = append(out, remoteOnlyValue(e, l[i], path)...)
			}
		}
		return out
	default:
		return nil
	}
}

// MarshalIndent renders a job definition as the canonical JSON text used for
// diffs and for files written by init. Object members are sorted by name.
func MarshalIndent(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}
