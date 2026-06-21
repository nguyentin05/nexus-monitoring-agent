# Branch Strategy

## Strategy
GitHub Flow

## Branches

| Branch | Description |
|---|---|
| `main` | Protected. Always stable and deployable. |
| `feat/<issue-id>-<subject>` | New feature |
| `fix/<issue-id>-<subject>` | Bug fix |
| `chore/<issue-id>-<subject>` | Maintenance, refactor, deps |
| `ci/<issue-id>-<subject>` | CI/CD changes |
| `docs/<issue-id>-<subject>` | Documentation |

## Flow

```
main → <type>/<issue-id>-<subject> → PR → merge to main
```

## Branch Protection (`main`)

- No direct push allowed
- PR required before merging
- All CI status checks must pass
- Minimum 1 approval required