# Canyon's ESLint image: ESLint 9.39.5 with eslint-plugin-security 4.2.0 and four rules fixed in its own
# config — the JavaScript and TypeScript flaws the vendored Opengrep rules miss (contracts/executor.md,
# #1376). Every package by exact version, everything they pull in resolved as the registry stood at
# 2026-10-02, install scripts off, on the digest-pinned base. The Docker executor builds it from this
# file on first use; on Kubernetes, build it and name it in CANYON_IMAGE_ESLINT (deploy/k8s/README.md):
#   docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/canyon-eslint:9.39.5 --push - < executor/eslint.Dockerfile
FROM node:22-slim@sha256:43ac6c60b8f89723f746e8a92ce91abd5017e627ce1ddfe4238355d3a30b772c
WORKDIR /opt/eslint
RUN npm install --no-audit --no-fund --ignore-scripts --before 2026-10-02T00:00:00Z \
      eslint@9.39.5 eslint-plugin-security@4.2.0 @typescript-eslint/parser@8.71.0 typescript@5.9.3 @microsoft/eslint-formatter-sarif@3.1.0 \
    && npm ls --all >/dev/null
# The rules, and only these: the tree's own config is never looked up and its inline comments are off.
RUN cat > /opt/eslint/eslint.config.js <<'CONF'
const security = require("eslint-plugin-security");
const parser = require("@typescript-eslint/parser");
module.exports = [{
  files: ["**/*.{js,cjs,mjs,jsx,ts,tsx,mts,cts}"],
  languageOptions: { parser, parserOptions: { ecmaFeatures: { jsx: true }, sourceType: "module" } },
  linterOptions: { reportUnusedDisableDirectives: "off", noInlineConfig: true },
  plugins: { security },
  rules: { "security/detect-child-process": "error", "security/detect-non-literal-fs-filename": "error", "no-eval": "error", "no-new-func": "error" },
}];
CONF
ENV HOME=/tmp
WORKDIR /scan/tree
ENTRYPOINT ["/opt/eslint/node_modules/.bin/eslint"]
