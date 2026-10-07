# Security Policy

The Lazarus team takes the security and integrity of database backups, restoration sandboxes, and cloud credentials seriously.

## Supported Versions

Only the latest major/minor releases receive active security updates and vulnerability patches.

| Version | Supported          |
| ------- | ------------------ |
| 1.x.x   | :white_check_mark: |
| < 1.0   | :x:                |

## Reporting a Vulnerability

If you discover a security vulnerability or security-sensitive defect in Lazarus, please **do not open a public issue**. Publicly disclosing flaws can expose production databases and backup archives to risk.

Please report vulnerabilities through one of the following channels:

1. **GitHub Security Advisories (Preferred)**:
   Navigate to the repository's [Security Advisories tab](https://github.com/qscgy5713/Lazarus/security/advisories) and click **"Report a vulnerability"**.

2. **Email Disclosure**:
   Send an encrypted or private report to `qscgy5713@gmail.com` with:
   - A clear description of the vulnerability and its potential impact.
   - Minimal reproducible steps or proof-of-concept (PoC) code/configuration.
   - Affected platforms or database engines (Postgres, MySQL, Redis, SQLite).

### Response Timeline

- **Initial Acknowledgement**: Within 48 hours of receipt.
- **Triage & Assessment**: Within 5 business days.
- **Fix & Public Advisory**: Coordinated disclosure after a patch is ready and tested across all supported platforms.

## Security Architecture & Best Practices

When operating Lazarus in production environments, consider the following design principles:

1. **Passphrase and Token Hygiene**:
   - Never commit `LAZARUS_GPG_PASSPHRASE` or `LAZARUS_API_KEY` to configuration files or git repositories. Always inject them via environment variables or secret managers (e.g., Vault, Kubernetes Secrets).
2. **Network Isolation**:
   - Disposable database sandboxes created by Lazarus run with `--network none` by default. Do not expose sandbox ports to external networks.
3. **Least Privilege Principle**:
   - S3 credentials used for pulling remote backups should only possess read-only permissions (`s3:GetObject`) scoped to the specific backup bucket and prefix.
   - The Lazarus Control Plane server container runs as a non-root user (`uid 10001`).
