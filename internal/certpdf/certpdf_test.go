package certpdf

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestGeneratePDF(t *testing.T) {
	data := CertificateData{
		Hostname:      "prod-drill-node-1",
		GeneratedAt:   time.Now().UTC(),
		TotalTargets:  3,
		PassedTargets: 3,
		SLAPercentage: 100.0,
		Targets: []TargetAudit{
			{
				Name:            "prod-postgres",
				Passed:          true,
				Stage:           "done",
				RestoreDuration: "1.2s",
				BackupSize:      "120 MB",
				ChecksPassed:    3,
				ChecksTotal:     3,
			},
			{
				Name:            "prod-mysql",
				Passed:          true,
				Stage:           "done",
				RestoreDuration: "800ms",
				BackupSize:      "45 MB",
				ChecksPassed:    2,
				ChecksTotal:     2,
			},
			{
				Name:            "prod-mongo",
				Passed:          true,
				Stage:           "done",
				RestoreDuration: "2.1s",
				BackupSize:      "320 MB",
				ChecksPassed:    1,
				ChecksTotal:     1,
			},
		},
	}

	pdfBytes := Generate(data)
	if len(pdfBytes) == 0 {
		t.Fatalf("Generate returned empty byte slice")
	}

	// Verify standard PDF header and trailer
	if !bytes.HasPrefix(pdfBytes, []byte("%PDF-1.4")) {
		t.Errorf("PDF output does not start with %%PDF-1.4 header")
	}

	content := string(pdfBytes)
	for _, expected := range []string{
		"LAZARUS DISASTER RECOVERY DRILL CERTIFICATE",
		"prod-drill-node-1",
		"prod-postgres",
		"prod-mysql",
		"prod-mongo",
		"CRYPTOGRAPHIC AUDIT SIGNATURE",
		"xref",
		"trailer",
		"%%EOF",
	} {
		if !strings.Contains(content, expected) {
			t.Errorf("PDF content missing expected token %q", expected)
		}
	}
}
