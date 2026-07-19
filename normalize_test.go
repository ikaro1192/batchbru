package batchbru

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/batch"
	"github.com/aws/aws-sdk-go-v2/service/batch/types"
)

// sampleDefinition exercises a wide slice of the schema: container properties,
// free-form maps whose keys must not be rewritten, enums, nested lists and
// numeric members.
const sampleDefinition = `{
  "jobDefinitionName": "example",
  "type": "container",
  "platformCapabilities": ["FARGATE"],
  "propagateTags": true,
  "schedulingPriority": 10,
  "parameters": {"InputPath": "s3://bucket/in", "Nested": "value", "Empty": ""},
  "tags": {"ManagedBy": "batchbru", "CostCenter": "1234"},
  "retryStrategy": {
    "attempts": 3,
    "evaluateOnExit": [{"onStatusReason": "Host EC2*", "action": "RETRY"}]
  },
  "timeout": {"attemptDurationSeconds": 3600},
  "containerProperties": {
    "image": "example.dkr.ecr.ap-northeast-1.amazonaws.com/app:latest",
    "command": ["python", "main.py"],
    "jobRoleArn": "arn:aws:iam::123456789012:role/job",
    "executionRoleArn": "arn:aws:iam::123456789012:role/exec",
    "privileged": false,
    "readonlyRootFilesystem": true,
    "environment": [{"name": "TZ", "value": "Asia/Tokyo"}],
    "resourceRequirements": [
      {"type": "VCPU", "value": "1"},
      {"type": "MEMORY", "value": "2048"}
    ],
    "fargatePlatformConfiguration": {"platformVersion": "LATEST"},
    "ephemeralStorage": {"sizeInGiB": 21},
    "logConfiguration": {
      "logDriver": "awslogs",
      "options": {"awslogs-group": "/aws/batch/job", "awslogs-stream-prefix": "example"}
    },
    "networkConfiguration": {"assignPublicIp": "ENABLED"}
  }
}`

func mustNormalize(t *testing.T, src string) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(src), &raw); err != nil {
		t.Fatalf("failed to parse the fixture: %v", err)
	}
	def, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return def
}

// TestNormalizeMatchesSDKWireFormat is the guard against the encoder drifting
// from the member names the Batch API actually uses. It sends the normalized
// document through the real SDK serializer and compares the resulting request
// body with what the encoder produced.
func TestNormalizeMatchesSDKWireFormat(t *testing.T) {
	def := mustNormalize(t, sampleDefinition)
	in, err := decodeRegisterInput(def)
	if err != nil {
		t.Fatalf("decodeRegisterInput: %v", err)
	}

	var body []byte
	client := batch.New(batch.Options{
		Region:      "ap-northeast-1",
		Credentials: aws.AnonymousCredentials{},
		HTTPClient: httpClientFunc(func(req *http.Request) (*http.Response, error) {
			body, _ = io.ReadAll(req.Body)
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"jobDefinitionName":"example","jobDefinitionArn":"arn","revision":1}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}),
	})
	if _, err := client.RegisterJobDefinition(context.Background(), in); err != nil {
		t.Fatalf("RegisterJobDefinition: %v", err)
	}

	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("failed to parse the captured request body: %v", err)
	}
	if !reflect.DeepEqual(normalizeNumbers(wire), normalizeNumbers(def)) {
		wireJSON, _ := json.MarshalIndent(wire, "", "  ")
		defJSON, _ := json.MarshalIndent(def, "", "  ")
		t.Errorf("the encoded definition differs from the SDK wire format\nwire:\n%s\nencoded:\n%s", wireJSON, defJSON)
	}
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// normalizeNumbers makes numbers comparable regardless of whether they came
// from encoding/json (float64) or from the reflective encoder (int64).
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			out[k] = normalizeNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalizeNumbers(e)
		}
		return out
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	default:
		return v
	}
}

// TestReadOnlyFieldsAreComplete checks that every member the API adds to a
// described job definition, and which cannot be sent back when registering, is
// listed in readOnlyFields.
func TestReadOnlyFieldsAreComplete(t *testing.T) {
	described := fieldNames(reflect.TypeOf(types.JobDefinition{}))
	registrable := fieldNames(reflect.TypeOf(batch.RegisterJobDefinitionInput{}))

	var extra []string
	for _, f := range described {
		if !contains(registrable, f) {
			extra = append(extra, f)
		}
	}
	sort.Strings(extra)
	want := append([]string{}, readOnlyFields...)
	sort.Strings(want)
	if !reflect.DeepEqual(extra, want) {
		t.Errorf("readOnlyFields is out of date: the API returns %v that cannot be registered, readOnlyFields is %v", extra, want)
	}
}

func fieldNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		out = append(out, lowerFirst(f.Name))
	}
	return out
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// TestRemoteToMapRoundTrip checks that a definition registered from a local
// file and then read back compares equal, which is what makes deploy idempotent.
func TestRemoteToMapRoundTrip(t *testing.T) {
	local := mustNormalize(t, sampleDefinition)
	in, err := decodeRegisterInput(local)
	if err != nil {
		t.Fatalf("decodeRegisterInput: %v", err)
	}

	// Rebuild what the API would return for that registration.
	remote := types.JobDefinition{
		JobDefinitionArn:           aws.String("arn:aws:batch:ap-northeast-1:123456789012:job-definition/example:7"),
		JobDefinitionName:          in.JobDefinitionName,
		Revision:                   aws.Int32(7),
		Status:                     aws.String(statusActive),
		Type:                       aws.String(string(in.Type)),
		ContainerOrchestrationType: types.OrchestrationTypeEcs,
		ContainerProperties:        in.ContainerProperties,
		Parameters:                 in.Parameters,
		PlatformCapabilities:       in.PlatformCapabilities,
		PropagateTags:              in.PropagateTags,
		RetryStrategy:              in.RetryStrategy,
		SchedulingPriority:         in.SchedulingPriority,
		Tags:                       in.Tags,
		Timeout:                    in.Timeout,
	}
	got, err := RemoteToMap(remote)
	if err != nil {
		t.Fatalf("RemoteToMap: %v", err)
	}
	got = PruneDefaults(got, local)
	if !reflect.DeepEqual(got, local) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		localJSON, _ := json.MarshalIndent(local, "", "  ")
		t.Errorf("a round trip through the API shape changed the definition\nremote:\n%s\nlocal:\n%s", gotJSON, localJSON)
	}
}

// TestPruneDefaults covers the members the API materialises for a definition
// that leaves them unset.
func TestPruneDefaults(t *testing.T) {
	local := map[string]any{
		"jobDefinitionName": "example",
		"type":              "container",
		"containerProperties": map[string]any{
			"image":      "app:latest",
			"privileged": false,
		},
	}
	remote := map[string]any{
		"jobDefinitionName":    "example",
		"type":                 "container",
		"propagateTags":        false,
		"schedulingPriority":   int64(0),
		"platformCapabilities": []any{"EC2"},
		"parameters":           map[string]any{},
		"containerProperties": map[string]any{
			"image":                  "app:latest",
			"privileged":             false,
			"readonlyRootFilesystem": false,
			"environment":            []any{},
			"mountPoints":            []any{},
			"volumes":                []any{},
			"secrets":                []any{},
		},
	}
	got := PruneDefaults(remote, local)
	if !reflect.DeepEqual(got, local) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("defaults were not pruned:\n%s", gotJSON)
	}
}

// TestPruneDefaultsKeepsRealDifferences makes sure pruning never hides a value
// that genuinely differs from the local definition.
func TestPruneDefaultsKeepsRealDifferences(t *testing.T) {
	local := map[string]any{"jobDefinitionName": "example"}
	remote := map[string]any{
		"jobDefinitionName":    "example",
		"platformCapabilities": []any{"FARGATE"},
		"retryStrategy":        map[string]any{"attempts": int64(3)},
		"propagateTags":        true,
	}
	got := PruneDefaults(remote, local)
	for _, k := range []string{"platformCapabilities", "retryStrategy", "propagateTags"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s was pruned even though it differs from the local definition", k)
		}
	}
}

// TestRemoteOnlyPaths checks the dotted paths reported for members that only
// the remote definition has, including inside nested objects and array
// elements (whose indexes are elided).
func TestRemoteOnlyPaths(t *testing.T) {
	remote := map[string]any{
		"a": int64(1),
		"b": map[string]any{"c": int64(1), "d": int64(2)},
		"e": []any{
			map[string]any{"f": int64(1)},
			map[string]any{"f": int64(1), "g": int64(2)},
		},
	}
	local := map[string]any{
		"a": int64(1),
		"b": map[string]any{"c": int64(1)},
		"e": []any{
			map[string]any{"f": int64(1)},
			map[string]any{"f": int64(1)},
		},
	}
	got := RemoteOnlyPaths(remote, local)
	want := []string{"b.d", "e.g"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RemoteOnlyPaths = %v, want %v", got, want)
	}
}

// TestNormalizeRejectsUnknownMembers checks that a misspelled member is
// reported rather than silently dropped.
func TestNormalizeRejectsUnknownMembers(t *testing.T) {
	var raw map[string]any
	json.Unmarshal([]byte(`{"jobDefinitionName":"x","type":"container","retryStrategey":{"attempts":1}}`), &raw)
	if _, err := Normalize(raw); err == nil {
		t.Fatal("expected an error for the misspelled member retryStrategey")
	}
}
