# Security Policy

## Scope

This repository contains infrastructure-facing services. Never commit real secrets, tokens, private keys, or production endpoints.

## Reporting a Vulnerability

Please report security issues privately to the maintainers. Do not open public issues for exploitable vulnerabilities.

Include:

- Affected component/file
- Reproduction steps
- Impact assessment
- Suggested fix (if available)

## Secret Handling Rules

- Use `.env.example` for placeholders only.
- Store real values in deployment secret managers or GitHub Actions secrets.
- Rotate credentials immediately if exposure is suspected.

## Operational Hardening Baseline

- Keep service ports bound to loopback when fronted by reverse proxy.
- Require `X-Service-Token` on private routes.
- Use least-privilege database credentials.
- Review dependency updates regularly.
