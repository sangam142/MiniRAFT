package main

import "testing"

func TestValidateGatewaySecurityConfig(t *testing.T) {
	tests := []struct {
		name                string
		chaosEnabled        bool
		chaosToken          string
		internalCommitToken string
		wantErr             bool
	}{
		{
			name:                "valid with chaos disabled",
			chaosEnabled:        false,
			chaosToken:          "",
			internalCommitToken: "internal-token",
			wantErr:             false,
		},
		{
			name:                "invalid without internal token",
			chaosEnabled:        false,
			chaosToken:          "",
			internalCommitToken: "",
			wantErr:             true,
		},
		{
			name:                "invalid chaos enabled without chaos token",
			chaosEnabled:        true,
			chaosToken:          "",
			internalCommitToken: "internal-token",
			wantErr:             true,
		},
		{
			name:                "valid chaos enabled with token",
			chaosEnabled:        true,
			chaosToken:          "chaos-token",
			internalCommitToken: "internal-token",
			wantErr:             false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGatewaySecurityConfig(tc.chaosEnabled, tc.chaosToken, tc.internalCommitToken)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}
