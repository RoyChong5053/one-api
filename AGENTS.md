# Agent Instructions - roychong/one-api fork

- **Language:** Code comments, logs, and commit messages should be in the language that best fits the context (English for code, Chinese for discussions when appropriate).
- **Project purpose:** one-api aggregates multiple upstream LLM API providers and exposes a unified OpenAI-compatible API.
- **Sensitive info:** Never leak API keys, secrets, or tokens in logs, outputs, or commits.
- **Deployment:** Development happens locally, then `git push` → `ssh m64` → `git pull` → build → `systemctl restart one-api`.
- **Build on m64:** `make build-frontend-modern` (if frontend changes) then `CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o build/bin/one-api .` then `sudo systemctl restart one-api`.
- **Error handling:** Wrap errors with context; never swallow errors.
- **Security:** Use constant-time comparisons for sensitive values; validate and sanitize all inputs.
- **Logging:** Use structured logging; never log secrets.
- **Testing:** Run `go test -race ./...` before deploying when relevant.
