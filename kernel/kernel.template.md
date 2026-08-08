<!--
  TIER A KERNEL BODY TEMPLATE

  Paste the rendered body into an active document in the server's `kernel`
  collection. Metadata such as title, status, owner, audience, tier, and
  triggers is stored separately by the server; do not add frontmatter here.

  Release cut enforces a 2,000-word cap and a 24-KiB byte backstop. Every line
  is paid for in every fresh agent session. If it is not required before the
  first tool call, put it in a Tier B `docs` document and link it below.

  Replace placeholders, delete comments, and keep the section shape.
-->

# {{ENV_NAME}} — agent operating contract

## Access paths

<!-- State the blessed mechanism and deliberate absences. -->

- {{SYSTEM_1}}: `{{ACCESS_COMMAND_1}}`
- {{SYSTEM_2}}: `{{ACCESS_COMMAND_2}}`

## Hard limits — never do

<!-- Name concrete protected resources, not broad categories. -->

- Never modify: {{PROTECTED_RESOURCES}}
- Never read secrets outside: {{SECRET_SCOPE}}
- Credentials live in {{SECRET_STORE}} — never in repositories, URLs, or chat.

## Test-sandbox contract

- Keep all test writes inside one fresh, uniquely named {{SANDBOX_UNIT}}
  (`{{SANDBOX_NAMING}}`).
- Check capacity before creating anything.
- Delete the sandbox at the end and show teardown proof:
  `{{TEARDOWN_PROOF}}`.

## Change control

- {{CHANGE_CONTROL_SUMMARY}}

## Stop conditions

- Outside the bounds above: stop and ask; do not improvise.
- {{ESCALATION_CONTACT_OR_CHANNEL}}

## Where to learn more

- Tier B procedures: `{{KNOWLEDGE_HOME}}/corpus/docs/`
- Tier C query: {{TIER_C_TOOLING}}
