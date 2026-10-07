// Package certpdf generates native compliance audit certificates in standard PDF format.
package certpdf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// TargetAudit summarizes one target for the certificate.
type TargetAudit struct {
	Name   string
	Passed bool
	// Verdict is the label printed in the table: PASS, FAIL, SLA MISS,
	// OVERDUE or MUTED. Empty falls back to PASS/FAIL from Passed.
	Verdict         string
	Stage           string
	RestoreDuration string
	BackupSize      string
	BackupAge       string
	ChecksPassed    int
	ChecksTotal     int
	Error           string
}

// CertificateData contains information needed to render the DR certificate.
type CertificateData struct {
	Hostname      string
	GeneratedAt   time.Time
	TotalTargets  int
	PassedTargets int
	// Eligible is how many targets count toward the SLA rate (total minus
	// muted ones in planned maintenance). Zero renders as NO DATA.
	Eligible      int
	SLAPercentage float64
	Targets       []TargetAudit
}

// Generate renders CertificateData into a valid PDF 1.4 document.
func Generate(data CertificateData) []byte {
	// Calculate digital audit fingerprint (SHA-256)
	hasher := sha256.New()
	hasher.Write([]byte(fmt.Sprintf("%s|%s|%d|%d|%.2f",
		data.Hostname,
		data.GeneratedAt.UTC().Format(time.RFC3339),
		data.TotalTargets,
		data.PassedTargets,
		data.SLAPercentage,
	)))
	for _, t := range data.Targets {
		hasher.Write([]byte(fmt.Sprintf("|%s|%t|%s|%s", t.Name, t.Passed, t.Stage, t.RestoreDuration)))
	}
	certFingerprint := strings.ToUpper(hex.EncodeToString(hasher.Sum(nil)))

	var stream bytes.Buffer

	// Background & Certificate Borders
	// Top Header Banner
	fmt.Fprintf(&stream, "0.08 0.18 0.36 rg\n") // Navy Blue
	fmt.Fprintf(&stream, "30 710 552 52 re f\n")
	// Outer Border
	fmt.Fprintf(&stream, "0.15 0.25 0.45 RG 2 w\n")
	fmt.Fprintf(&stream, "30 30 552 732 re S\n")
	// Inner Border
	fmt.Fprintf(&stream, "0.85 0.88 0.92 RG 1 w\n")
	fmt.Fprintf(&stream, "35 35 542 722 re S\n")

	// Header Text
	fmt.Fprintf(&stream, "BT\n")
	fmt.Fprintf(&stream, "/F2 18 Tf 1 1 1 rg 50 730 Td (%s) Tj\n", escapePDF("LAZARUS DISASTER RECOVERY DRILL CERTIFICATE"))
	fmt.Fprintf(&stream, "ET\n")

	// Subtitle & Metadata
	fmt.Fprintf(&stream, "BT\n")
	fmt.Fprintf(&stream, "/F1 9 Tf 0.3 0.35 0.45 rg 50 685 Td (%s) Tj\n", escapePDF("OFFICIAL AUDIT ATTESTATION - AUTOMATED DATABASE RESTORATION PROOF"))
	fmt.Fprintf(&stream, "/F1 8 Tf 0.4 0.4 0.4 rg 50 670 Td (%s) Tj\n", escapePDF(fmt.Sprintf("Audited Host: %s  |  Timestamp: %s  |  Standard: ISO 27001 / SOC 2 DR", data.Hostname, data.GeneratedAt.UTC().Format(time.RFC3339))))
	fmt.Fprintf(&stream, "ET\n")

	// Summary KPI Box
	fmt.Fprintf(&stream, "0.96 0.97 0.98 rg 50 595 512 60 re f\n")
	fmt.Fprintf(&stream, "0.82 0.85 0.90 RG 1 w 50 595 512 60 re S\n")

	statusWord := "COMPLIANT"
	if data.TotalTargets == 0 || data.PassedTargets < data.TotalTargets || data.SLAPercentage < 100.0 {
		statusWord = "NON-COMPLIANT"
	}

	slaText := "N/A"
	if data.Eligible > 0 {
		slaText = fmt.Sprintf("%.1f%%", data.SLAPercentage)
	}

	fmt.Fprintf(&stream, "BT\n")
	fmt.Fprintf(&stream, "/F2 10 Tf 0.2 0.25 0.3 rg 65 635 Td (%s) Tj\n", escapePDF("TOTAL TARGETS"))
	fmt.Fprintf(&stream, "/F2 16 Tf 0.1 0.2 0.4 rg 65 610 Td (%s) Tj\n", escapePDF(fmt.Sprintf("%d", data.TotalTargets)))

	fmt.Fprintf(&stream, "/F2 10 Tf 0.2 0.25 0.3 rg 180 635 Td (%s) Tj\n", escapePDF("HEALTHY TARGETS"))
	fmt.Fprintf(&stream, "/F2 16 Tf 0.15 0.55 0.25 rg 180 610 Td (%s) Tj\n", escapePDF(fmt.Sprintf("%d", data.PassedTargets)))

	fmt.Fprintf(&stream, "/F2 10 Tf 0.2 0.25 0.3 rg 310 635 Td (%s) Tj\n", escapePDF("SLA COMPLIANCE RATE"))
	fmt.Fprintf(&stream, "/F2 16 Tf 0.1 0.2 0.4 rg 310 610 Td (%s) Tj\n", escapePDF(slaText))

	fmt.Fprintf(&stream, "/F2 10 Tf 0.2 0.25 0.3 rg 450 635 Td (%s) Tj\n", escapePDF("AUDIT STATUS"))
	if statusWord == "COMPLIANT" {
		fmt.Fprintf(&stream, "/F2 14 Tf 0.15 0.55 0.25 rg 450 610 Td (%s) Tj\n", escapePDF(statusWord))
	} else {
		fmt.Fprintf(&stream, "/F2 14 Tf 0.75 0.15 0.15 rg 450 610 Td (%s) Tj\n", escapePDF(statusWord))
	}
	fmt.Fprintf(&stream, "ET\n")

	// Table Section Header
	fmt.Fprintf(&stream, "BT\n")
	fmt.Fprintf(&stream, "/F2 11 Tf 0.15 0.2 0.3 rg 50 570 Td (%s) Tj\n", escapePDF("VERIFIED TARGET AUDIT TRAIL"))
	fmt.Fprintf(&stream, "ET\n")

	// Table Header Bar
	fmt.Fprintf(&stream, "0.18 0.28 0.45 rg 50 545 512 18 re f\n")
	fmt.Fprintf(&stream, "BT\n")
	fmt.Fprintf(&stream, "/F2 8 Tf 1 1 1 rg\n")
	fmt.Fprintf(&stream, "60 550 Td (%s) Tj\n", escapePDF("TARGET"))
	fmt.Fprintf(&stream, "170 550 Td (%s) Tj\n", escapePDF("STATUS"))
	fmt.Fprintf(&stream, "240 550 Td (%s) Tj\n", escapePDF("STAGE"))
	fmt.Fprintf(&stream, "310 550 Td (%s) Tj\n", escapePDF("RESTORE TIME"))
	fmt.Fprintf(&stream, "400 550 Td (%s) Tj\n", escapePDF("BACKUP SIZE"))
	fmt.Fprintf(&stream, "480 550 Td (%s) Tj\n", escapePDF("CHECKS"))
	fmt.Fprintf(&stream, "ET\n")

	// Table Rows
	y := 528
	for i, t := range data.Targets {
		if y < 140 {
			break // Prevent overflow on 1-page certificate
		}
		if i%2 == 1 {
			fmt.Fprintf(&stream, "0.97 0.98 0.99 rg 50 %d 512 16 re f\n", y-2)
		}
		fmt.Fprintf(&stream, "0.90 0.92 0.94 RG 0.5 w 50 %d 512 16 re S\n", y-2)

		targetStatus := t.Verdict
		if targetStatus == "" {
			if t.Passed {
				targetStatus = "PASS"
			} else {
				targetStatus = "FAIL"
			}
		}
		checksStr := fmt.Sprintf("%d/%d", t.ChecksPassed, t.ChecksTotal)
		if t.ChecksTotal == 0 {
			checksStr = "-"
		}

		fmt.Fprintf(&stream, "BT\n")
		fmt.Fprintf(&stream, "/F2 8 Tf 0.2 0.2 0.25 rg 60 %d Td (%s) Tj\n", y+2, escapePDF(truncateStr(t.Name, 22)))

		switch targetStatus {
		case "PASS":
			fmt.Fprintf(&stream, "/F2 8 Tf 0.15 0.55 0.25 rg 170 %d Td (%s) Tj\n", y+2, escapePDF(targetStatus))
		case "MUTED":
			fmt.Fprintf(&stream, "/F2 8 Tf 0.5 0.5 0.5 rg 170 %d Td (%s) Tj\n", y+2, escapePDF(targetStatus))
		case "SLA MISS", "OVERDUE":
			fmt.Fprintf(&stream, "/F2 8 Tf 0.85 0.45 0.1 rg 170 %d Td (%s) Tj\n", y+2, escapePDF(targetStatus))
		default: // FAIL
			fmt.Fprintf(&stream, "/F2 8 Tf 0.75 0.15 0.15 rg 170 %d Td (%s) Tj\n", y+2, escapePDF(targetStatus))
		}

		fmt.Fprintf(&stream, "/F1 8 Tf 0.3 0.35 0.4 rg 240 %d Td (%s) Tj\n", y+2, escapePDF(t.Stage))
		fmt.Fprintf(&stream, "/F1 8 Tf 0.3 0.35 0.4 rg 310 %d Td (%s) Tj\n", y+2, escapePDF(t.RestoreDuration))
		fmt.Fprintf(&stream, "/F1 8 Tf 0.3 0.35 0.4 rg 400 %d Td (%s) Tj\n", y+2, escapePDF(t.BackupSize))
		fmt.Fprintf(&stream, "/F1 8 Tf 0.3 0.35 0.4 rg 480 %d Td (%s) Tj\n", y+2, escapePDF(checksStr))
		fmt.Fprintf(&stream, "ET\n")

		y -= 17
	}

	// Digital Signature & Audit Seal Box
	fmt.Fprintf(&stream, "0.96 0.97 0.98 rg 50 70 512 55 re f\n")
	fmt.Fprintf(&stream, "0.80 0.85 0.90 RG 1 w 50 70 512 55 re S\n")

	fmt.Fprintf(&stream, "BT\n")
	fmt.Fprintf(&stream, "/F2 8 Tf 0.2 0.25 0.35 rg 60 108 Td (%s) Tj\n", escapePDF("CRYPTOGRAPHIC AUDIT SIGNATURE (SHA-256):"))
	fmt.Fprintf(&stream, "/F1 7 Tf 0.3 0.3 0.3 rg 60 96 Td (%s) Tj\n", escapePDF(certFingerprint))
	fmt.Fprintf(&stream, "/F1 7 Tf 0.4 0.45 0.5 rg 60 78 Td (%s) Tj\n", escapePDF("This certificate validates that database restores were executed in isolated disposable sandboxes and verified via query assertions."))
	fmt.Fprintf(&stream, "ET\n")

	// Footer
	fmt.Fprintf(&stream, "BT\n")
	fmt.Fprintf(&stream, "/F1 7 Tf 0.5 0.5 0.5 rg 50 45 Td (%s) Tj\n", escapePDF(fmt.Sprintf("Generated by Lazarus DR Engine | Verify at https://github.com/chen-yuju/Lazarus | %s", data.GeneratedAt.UTC().Format(time.RFC3339))))
	fmt.Fprintf(&stream, "ET\n")

	streamBytes := stream.Bytes()

	// PDF Structure Assembly
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	offsets := make([]int, 7) // 1-indexed (0 unused)

	// Object 1: Catalog
	offsets[1] = pdf.Len()
	pdf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")

	// Object 2: Pages
	offsets[2] = pdf.Len()
	pdf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")

	// Object 3: Page
	offsets[3] = pdf.Len()
	pdf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R /F2 5 0 R >> >> /Contents 6 0 R >>\nendobj\n")

	// Object 4: Font F1 (Helvetica)
	offsets[4] = pdf.Len()
	pdf.WriteString("4 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")

	// Object 5: Font F2 (Helvetica-Bold)
	offsets[5] = pdf.Len()
	pdf.WriteString("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold >>\nendobj\n")

	// Object 6: Stream Content
	offsets[6] = pdf.Len()
	pdf.WriteString(fmt.Sprintf("6 0 obj\n<< /Length %d >>\nstream\n", len(streamBytes)))
	pdf.Write(streamBytes)
	pdf.WriteString("\nendstream\nendobj\n")

	// Xref Table
	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 7\n")
	pdf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= 6; i++ {
		pdf.WriteString(fmt.Sprintf("%010d 00000 n \n", offsets[i]))
	}

	// Trailer
	pdf.WriteString(fmt.Sprintf("trailer\n<< /Size 7 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset))

	return pdf.Bytes()
}

func escapePDF(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "(", "\\(")
	s = strings.ReplaceAll(s, ")", "\\)")
	return s
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
