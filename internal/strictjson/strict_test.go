package strictjson

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	for _, body := range []string{
		`{"name":"first","name":"second"}`,
		`{"config":{"runtime":"go","runtime":"node"}}`,
		`{"items":[{"name":"first","name":"second"}]}`,
		`{"name":"first","\u006eame":"second"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if err := Validate([]byte(body)); !errors.Is(err, ErrDuplicateKey) {
				t.Fatalf("Validate() error = %v, want ErrDuplicateKey", err)
			}
		})
	}
}

func TestDecodeRequiresOneValidKnownFieldDocument(t *testing.T) {
	type input struct {
		Name string `json:"name"`
	}
	for _, body := range []string{`{"name":"demo"}{}`, `{"unknown":true}`, `{"name":`} {
		var result input
		if err := Decode([]byte(body), &result); err == nil {
			t.Errorf("Decode(%q) unexpectedly succeeded", body)
		}
	}
	var result input
	if err := Decode([]byte(`{"name":"demo"}`), &result); err != nil || result.Name != "demo" {
		t.Fatalf("Decode() = %+v, %v", result, err)
	}
}

func TestValidateBoundsNestingAndRejectsMalformedClosings(t *testing.T) {
	for _, body := range []string{`[}`, `{"x":]`, `[1}}`, `{"x":1]`} {
		if err := Validate([]byte(body)); err == nil {
			t.Errorf("Validate(%q) unexpectedly succeeded", body)
		}
	}
	for _, body := range []string{`null`, `true`, `42`, `"text"`, `[]`, `{}`} {
		if err := Validate([]byte(body)); err != nil {
			t.Errorf("Validate(%q) = %v", body, err)
		}
	}
	tooDeep := strings.Repeat("[", maxNestingDepth+1) + "0" + strings.Repeat("]", maxNestingDepth+1)
	if err := Validate([]byte(tooDeep)); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("Validate(deep document) = %v, want ErrTooDeep", err)
	}
	deepButAllowed := strings.Repeat("[", maxNestingDepth) + "0" + strings.Repeat("]", maxNestingDepth)
	if err := Validate([]byte(deepButAllowed)); err != nil {
		t.Fatalf("Validate(maximum-depth document) = %v", err)
	}
}
