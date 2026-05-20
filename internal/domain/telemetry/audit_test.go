package telemetry

import "testing"

func TestAuditDecisionHelpers(t *testing.T) {
	rec := NewAuditEvent(NewEventCommon(CommonInput{}))
	MarkAuthSuccess(rec)
	SetResource(rec, "llm_model", "coder")
	MarkPolicyAllowed(rec)
	if rec.AuthResult != AuthResultSuccess || rec.PolicyResult != PolicyResultAllowed {
		t.Fatalf("allowed decision = %+v", rec)
	}
	MarkAuthFailure(rec, AuthErrorFormat)
	if rec.AuthResult != AuthResultFailure || rec.AuthError != AuthErrorFormat || rec.DenyReason != DenyReasonAuth {
		t.Fatalf("auth failure = %+v", rec)
	}
	MarkPolicyDenied(rec, DenyReasonModelNotAllowed)
	if rec.PolicyResult != PolicyResultDenied || rec.DenyReason != DenyReasonModelNotAllowed {
		t.Fatalf("policy denial = %+v", rec)
	}
}
