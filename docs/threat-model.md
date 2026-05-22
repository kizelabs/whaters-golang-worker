# Threat Model (WhatsApp Routing Service)

## Assets

- WhatsApp session/auth state
- Service token (`X-Service-Token`)
- Database credentials
- Inbound/outbound message payloads in transit

## Trust Boundaries

- Internet -> Reverse proxy (Caddy)
- Reverse proxy -> whatsapp-service (loopback)
- whatsapp-service -> Postgres
- whatsapp-service -> Cloudflare Worker webhook

## Main Threats

- Credential leakage through source control
- Unauthorized API access to private routes
- Lateral movement via exposed container ports
- Session ownership conflicts in HA mode
- MITM risk if TLS is not terminated correctly

## Mitigations Implemented

- Private routes protected by `X-Service-Token`
- Loopback port binding in compose for proxy-fronted mode
- HA lease ownership with fencing token in Postgres
- `.env` and runtime data excluded by `.gitignore`

## Residual Risks / Required Operations

- Rotate tokens after any suspected exposure
- Keep TLS termination enforced at reverse proxy
- Enable GitHub secret scanning + push protection
- Use least-privilege DB role and periodic key rotation
