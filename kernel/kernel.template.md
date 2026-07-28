---
title: "{{ENV_NAME}} agent kernel"
status: draft
owner: "{{OWNER}}"
audience: agent
tier: A
---

<!--
  KERNEL TEMPLATE — the always-injected contract (Tier A).

  Hard cap: ~1–2k tokens. Every line here is paid for in EVERY session of
  EVERY agent on EVERY host. If it isn't a rule the agent must know before
  its first tool call, it belongs in a Tier B doc — link it under
  "Where to learn more".

  Replace {{placeholders}}; delete the comments; keep the section shape.
-->

# {{ENV_NAME}} — agent operating contract

## Access paths
<!-- The blessed way to reach each system. State the mechanism AND the
     deliberate absences (e.g. "no local kubeconfig by design"). -->
- {{SYSTEM_1}}: `{{ACCESS_COMMAND_1}}`
- {{SYSTEM_2}}: `{{ACCESS_COMMAND_2}}`

## Hard limits — never do
<!-- The never-touch list. Name concrete resources, not categories. -->
- Never modify: {{PROTECTED_RESOURCES}}
- Never read secrets outside: {{SECRET_SCOPE}}
- Credentials live in {{SECRET_STORE}} — never in repos, URLs, or chat.

## Test-sandbox contract
<!-- What "experiments are allowed" means, precisely. -->
- All test writes stay inside ONE fresh, uniquely named {{SANDBOX_UNIT}}
  (naming: `{{SANDBOX_NAMING}}`).
- Check capacity/headroom before creating anything.
- When done: delete the sandbox and SHOW it gone ({{TEARDOWN_PROOF}}).

## Change control
- {{CHANGE_CONTROL_SUMMARY}}
  <!-- e.g. "All deploys via <pipeline>; direct applies to shared systems
       are forbidden; approval tiers are defined in <doc>." -->

## Stop conditions
- Outside the bounds above → stop and ask; don't improvise.
- {{ESCALATION_CONTACT_OR_CHANNEL}}

## Where to learn more (Tier B/C)
- Full guides: `{{KNOWLEDGE_HOME}}/corpus/docs/`
- Query: {{TIER_C_TOOLING}}
