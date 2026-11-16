# Superpowers skills: where files go

Agents in this repo may use the Superpowers skills (brainstorming, writing-plans, executing-plans and others). By default those skills write to `docs/superpowers/specs/` and `docs/superpowers/plans/`.

This repo does not use those paths.

- Specs (design documents) go in `docs/process/specs/`.
- Implementation plans go in `docs/process/plans/`.
- File names stay as the skills suggest: `YYYY-MM-DD-<topic>-design.md` for specs, `YYYY-MM-DD-<topic>.md` for plans.

Never create a folder or file whose name contains "superpowers". The tooling is an internal detail. The repo layout should describe the work, not the tool that helped write it.
