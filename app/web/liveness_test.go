package web

import (
	"net/http"
	"testing"
	"time"
)

func TestEvaluateLiveness_HealthyClearsTimer(t *testing.T) {
	now := time.Now()
	since := now.Add(-10 * time.Minute)

	code, newSince, stuck := evaluateLiveness(true, &since, now, time.Minute)

	if code != http.StatusOK {
		t.Errorf("healthy must return 200, got %d", code)
	}
	if newSince != nil {
		t.Error("healthy must clear the unhealthy timestamp")
	}
	if stuck != 0 {
		t.Errorf("healthy must report 0 stuck time, got %v", stuck)
	}
}

func TestEvaluateLiveness_WithinGraceStaysOK(t *testing.T) {
	now := time.Now()
	since := now.Add(-30 * time.Second)

	code, newSince, stuck := evaluateLiveness(false, &since, now, 4*time.Minute)

	if code != http.StatusOK {
		t.Errorf("within grace must return 200, got %d", code)
	}
	if newSince == nil || !newSince.Equal(since) {
		t.Error("within grace must keep the original unhealthy timestamp")
	}
	if stuck != 30*time.Second {
		t.Errorf("expected 30s stuck, got %v", stuck)
	}
}

func TestEvaluateLiveness_PastGraceFails(t *testing.T) {
	now := time.Now()
	since := now.Add(-5 * time.Minute)

	code, _, _ := evaluateLiveness(false, &since, now, 4*time.Minute)

	if code != http.StatusServiceUnavailable {
		t.Errorf("past grace must return 503, got %d", code)
	}
}

func TestEvaluateLiveness_FirstUnhealthyStartsTimer(t *testing.T) {
	now := time.Now()

	code, newSince, stuck := evaluateLiveness(false, nil, now, time.Minute)

	if code != http.StatusOK {
		t.Errorf("first unhealthy tick must return 200, got %d", code)
	}
	if newSince == nil || !newSince.Equal(now) {
		t.Error("first unhealthy tick must start the timer at now")
	}
	if stuck != 0 {
		t.Errorf("expected 0 stuck on first tick, got %v", stuck)
	}
}
