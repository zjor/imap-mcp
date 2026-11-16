# imap-mcp

A stateless MCP server in Go that gives AI agents safe access to an IMAP mailbox. Agents can read, search and download. The only write is a draft. The server never sends, deletes or changes existing mail.

The design is in [docs/process/specs/2026-11-16-imap-mcp-design.md](docs/process/specs/2026-11-16-imap-mcp-design.md).

## Conventions

- [docs/conventions/superpowers.md](docs/conventions/superpowers.md): where specs and plans go. Never use "superpowers" in folder names.
