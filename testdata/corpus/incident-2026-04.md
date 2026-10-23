---
id: incident-2026-04
title: Order API outage postmortem
updated: 2026-04-14
---
# Order API outage postmortem

On 2026-04-12 the order API was down for 47 minutes because a TLS certificate expired.

Certificate expiry is now monitored, with alerts 14 days before expiry.
