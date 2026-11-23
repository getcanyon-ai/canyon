// The Canyon docs: Starlight, built to static files by `npm run build` (docs/README.md).
import { readFileSync, realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import { satteri } from "@astrojs/markdown-satteri";
import starlightLinksValidator from "starlight-links-validator";
import starlightLlmsTxt from "starlight-llms-txt";
import starlightDotMd from "starlight-dot-md";
import { mermaid } from "./src/plugins/mermaid.mjs";
import { diagram } from "./src/plugins/diagram.mjs";
import { instructions } from "./src/llms.mjs";

const fonts = fileURLToPath(new URL("./src/fonts", import.meta.url));
const releases = "https://github.com/getcanyon-ai/canyon-releases"; // the public download repository; Canyon's own is private
// The build Vercel runs is the one that ships (scripts/publish-docs.sh's vercel.json sets the address): it never
// links llms.txt and the sitemap to localhost.
if (process.env.VERCEL && !process.env.CANYON_DOCS_URL) throw new Error("CANYON_DOCS_URL is unset: a build that ships needs the site's address");
// The Canyon release the site documents, named once in every page's header (src/components/SiteTitle.astro).
// scripts/publish-docs.sh sets it from the release tag it publishes; any other build documents main.
const version = process.env.CANYON_DOCS_VERSION ?? "main (unreleased)";
if (process.env.CANYON_DOCS_VERSION !== undefined && !/^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/.test(version)) throw new Error(`CANYON_DOCS_VERSION is ${JSON.stringify(version)}: a release tag such as v1.2.3`);
if (process.env.VERCEL && process.env.CANYON_DOCS_VERSION === undefined) throw new Error("CANYON_DOCS_VERSION is unset: a build that ships names its release");
// The path the site is served under: /docs on getcanyon.ai, which scripts/publish-docs.sh sets, and whose links it
// writes into its copy of the pages (scripts/base-links.mjs); unset, the root, as in a local build.
const base = process.env.CANYON_DOCS_BASE;
if (base !== undefined && !/^\/[a-z0-9-]+(\/[a-z0-9-]+)*$/.test(base)) throw new Error(`CANYON_DOCS_BASE is ${JSON.stringify(base)}: a path such as /docs`);

export default defineConfig({
  // Where the site is served, for llms.txt's links and the sitemap; unset, astro preview's own address.
  site: process.env.CANYON_DOCS_URL ?? "http://localhost:4321",
  base,
  markdown: { processor: satteri({ mdastPlugins: [mermaid, diagram(base)] }) },
  // The fonts are the dashboard's own: src/fonts links to web/src/fonts, and tokens.css names them under this alias.
  vite: {
    define: { __CANYON_DOCS_VERSION__: JSON.stringify(version) },
    resolve: { alias: { "@canyon-fonts": fonts } },
    server: { fs: { allow: [".", realpathSync(fonts)] } },
  },
  integrations: [
    starlight({
      title: "Canyon docs",
      description: "Canyon reviews every deploy your coding agents make, and runs the apps that pass.",
      social: [{ icon: "github", label: "Downloads", href: releases }],
      customCss: ["./src/styles/tokens.css", "./src/styles/canyon.css", "./src/styles/diagrams.css", "./src/styles/brand.css"],
      // Shows a wide mermaid diagram's "Scroll sideways" hint exactly while its frame scrolls.
      head: [{ tag: "script", content: readFileSync(fileURLToPath(new URL("./src/scripts/diagram-scroll.js", import.meta.url)), "utf8") }],
      components: { SiteTitle: "./src/components/SiteTitle.astro", SocialIcons: "./src/components/SocialIcons.astro" },
      lastUpdated: false,
      // Grouped by who reads: developers (Get started, Build and ship apps), operators (Install and run
      // Canyon), admins (Administer Canyon) and platform teams (Integrate your platform). A group is the
      // sidebar's alone: every page keeps its URL wherever it sits.
      sidebar: [
        {
          label: "Get started",
          items: [
            "start/what-is-canyon",
            "start/try-it-on-your-laptop",
            "start/take-the-tour",
            {
              label: "Use it from your coding agent",
              items: [
                { slug: "start/use-it-from-your-coding-agent", label: "Claude Code" },
                "start/agents/cursor",
                "start/agents/codex",
                "start/agents/other-agents",
                "start/connect-your-agent",
              ],
            },
            "start/deploy-on-your-companys-canyon",
          ],
        },
        {
          label: "Build and ship apps",
          items: [
            "build/fix-or-approve",
            "build/production",
            "build/secrets",
            "build/databases",
            "build/your-apps-data",
            "build/sign-in-in-your-app",
            "build/share",
            "build/github",
            "reference/troubleshooting",
          ],
        },
        {
          label: "Install and run Canyon",
          // Each guide's tier, as What Canyon is draws them: try it, one server, production.
          items: [
            { slug: "self-host/helm", badge: "Try it" },
            { slug: "self-host/docker", badge: "One server" },
            { slug: "self-host/k3s", badge: "One server" },
            { slug: "self-host/kubernetes", badge: "Production" },
            "self-host/front-door",
            "org/set-up",
            "self-host/configuration",
            "self-host/backup-and-upgrade",
          ],
        },
        {
          label: "Administer Canyon",
          items: ["org/first-boot", "org/who-can-do-what", "org/guardrails", "org/playbooks", "org/outbound", "org/integrations", "org/audit-trail", "org/dashboard", "org/usage-reports", "org/licence"],
        },
        {
          label: "Integrate your platform",
          items: [
            "self-host/hand-off",
            "self-host/hand-off-argo-cd",
            "self-host/hand-off-flux",
            "self-host/hand-off-external-secrets",
            "reference/contracts",
            "reference/finding-format",
          ],
        },
        { label: "Concepts", items: ["concepts/how-canyon-works", "concepts/modes"] },
        { label: "Reference", items: ["reference/cli", "reference/canyon-toml", "reference/api", "reference/troubleshooting", "reference/ask-for-a-feature"] },
        {
          label: "Rules",
          collapsed: true,
          items: [
            { slug: "rules", label: "All rules" },
            "rules/secrets.client-bundle",
            "rules/sql.string-concat",
            "rules/prod.clean-gate",
            "rules/prod.human-approval",
            "rules/auth.unauthenticated-route",
            "rules/deps.known-vuln",
            "rules/stack.blessed",
            "rules/owner.required",
            "rules/perf.unbounded-refetch",
            "rules/design.org-system",
            "rules/code.injection",
            "rules/iac.misconfig",
            "rules/ai.sql-string-concat",
            "rules/ai.code-injection",
            "rules/sarif.checkov",
            "rules/sarif.eslint",
            "rules/sarif.gitleaks",
            "rules/sarif.grype",
            "rules/sarif.opengrep",
            "rules/sarif.trivy",
            "rules/platform.known-vuln",
            "rules/prod.egress-allowed",
          ],
        },
      ],
      plugins: [
        starlightLinksValidator(),
        starlightDotMd(),
        starlightLlmsTxt({
          projectName: "Canyon",
          description: "Canyon reviews and runs apps that coding agents build. Every deploy is reviewed; a blocking finding stops the build; a person promotes to production.",
          details: instructions,
          rawContent: true,
        }),
      ],
    }),
  ],
});
