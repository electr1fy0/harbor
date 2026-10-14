package dashboard

import (
	"net/http"
	"testing"
)

func TestAPIStrictJSONKeysAndNullRollbackNeverReachControl(t *testing.T) {
	control := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid input reached the control API") })
	api := NewAPI(control)
	for _, tc := range []struct{ name, operation, body string }{
		{"null release", "rollbackApp", `{"name":"demo","releaseID":null}`},
		{"case folded name", "getApp", `{"Name":"demo"}`},
		{"case folded map", "configureApp", `{"name":"demo","Environment":{"TOKEN":null},"domains":["example.com"]}`},
		{"case folded option", "configureApp", `{"name":"demo","CpuQuota":null,"domains":["example.com"]}`},
		{"nested case folded config", "inspectProject", `{"repoURL":"https://github.com/owner/repo","config":{"Runtime":"go"}}`},
		{"case folded environment submission", "submitOnboardingJob", `{"name":"demo","repoURL":"https://github.com/owner/repo","Environment":{"TOKEN":null}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := dashboardRequest(api, http.MethodPost, tc.operation, tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}
