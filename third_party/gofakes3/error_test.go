package gofakes3

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	xml "github.com/minio/xxml"
)

func TestSlowDownError(t *testing.T) {
	code := ErrSlowDown

	if got, want := code.Message(), "Please reduce your request rate."; got != want {
		t.Errorf("Message() = %q, want %q", got, want)
	}
}

func TestUnknownErrorCodeDefaultsToInternalServerError(t *testing.T) {
	code := ErrorCode("UnknownError")
	if got, want := code.Status(), http.StatusInternalServerError; got != want {
		t.Fatalf("Status() = %d, want %d", got, want)
	}
	resp := ensureErrorResponse(code, "")
	if got, want := resp.(*ErrorResponse).Message, string(code); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestSlowDownHTTPResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	(&GoFakeS3{}).httpError(recorder, request, ErrSlowDown)

	if got, want := recorder.Code, http.StatusServiceUnavailable; got != want {
		t.Errorf("status code = %d, want %d", got, want)
	}

	var resp ErrorResponse
	if err := xml.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got, want := resp.Code, ErrSlowDown; got != want {
		t.Errorf("error code = %q, want %q", got, want)
	}
	if got, want := resp.Message, ErrSlowDown.Message(); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestErrorCustomResponseMarshalsAsExpected(t *testing.T) {
	resp := requestTimeTooSkewed(time.Time{}, 123)
	out, err := xml.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	// This is slightly brittle, but it does ensure that the embedded struct
	// has its fields merged with the element rather than being nested:
	expected := `<Error>` +
		`<Code>RequestTimeTooSkewed</Code>` +
		`<Message>The difference between the request time and the current time is too large</Message>` +
		`<ServerTime>0001-01-01T00:00:00Z</ServerTime>` +
		`<MaxAllowedSkewMilliseconds>0</MaxAllowedSkewMilliseconds>` +
		`</Error>`

	if string(out) != expected {
		t.Fatalf("expected:\n%s\nfound:\n%s", expected, out)
	}
}
