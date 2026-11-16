// What llms.txt tells an agent before its index: the installed skill's own rules (cli/skill/canyon.md),
// so an agent that has only read the docs behaves as one with the skill installed.
export const instructions = `## Instructions for AI coding agents

- Deploy from the app's root directory with \`canyon deploy\`. Its output ends in one of six states: \`RESULT: PASSED\`, \`RESULT: BLOCKED\`, \`RESULT: APPROVED\`, \`RESULT: FAILED\`, \`RESULT: STILL RUNNING\` or \`RESULT: UNKNOWN\`.
- \`RESULT: STILL RUNNING\`: the deploy was still running when Canyon last answered. Follow its NEXT STEPS, and never deploy again or take the app offline because of it.
- \`RESULT: UNKNOWN\`: Canyon said the run ended, and this command couldn't read how. Follow its NEXT STEPS, and never deploy again or take the app offline because of it.
- \`RESULT: BLOCKED\`: for each finding with \`blocks: yes\`, make the code match its \`required end state\`, obey its \`do not:\` list, then run \`canyon check\`, which runs the same review without deploying. Deploy again once it passes.
- Never add an ignore comment, a suppression pragma or \`--force\`, and never move a secret into a \`.env\` file, a comment, a config file or a client-side variable to make a finding go away. Store a value the code needs with \`canyon secret set NAME\`.
- You cannot approve a blocking finding, and neither can your user. When one won't clear, show it to your user: an admin or a security responder can approve it with a reason on the app's page in Canyon, never one of a review they started.
- After two fix-and-check rounds that still end in \`RESULT: BLOCKED\`, stop and ask your user how to proceed.
- Production: run \`canyon deploy --env prod\` only when your user asks for it. If it ends \`WAITING FOR APPROVAL\`, give your user the approval link and stop.
- Install the full instructions for your agent with \`canyon skill install\`. Any page of these docs is available as Markdown: add \`.md\` to its address.`;
