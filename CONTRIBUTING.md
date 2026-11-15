# Contributing

Thanks for helping. Bug reports from real setups are the most useful thing you
can send: every storage provider, CloudNativePG version and cluster layout
finds different failures.

## Reporting a problem

Open an issue with the bug template. Include `kubectl get restorecheck -o yaml`
for the affected check, the controller logs
(`kubectl -n cnpg-restore-check logs deploy/cnpg-restore-check`), and your
CloudNativePG and plugin-barman-cloud versions. Remove credentials and bucket
names you don't want public.

If you think you found a security problem (for example, a way for a restore
check to write to the source archive), do not open a public issue. Use
GitHub's private vulnerability reporting on this repository instead.

## Making a change

1. For anything bigger than a small fix, open an issue first so we can agree on
   the approach.
2. Build and test:

   ```sh
   make test            # unit tests
   make test-alerts     # promtool tests for the alert rules
   make manifests       # after changing api/ or RBAC markers; commit the result
   make lint-chart
   ```

   `make e2e` runs the end-to-end suite against your current kubectl context
   (see [test/e2e/README.md](test/e2e/README.md)). CI runs it on kind for every
   pull request, so you don't have to run it locally.
3. Add or update tests with the change. Behaviour changes need a unit test; a
   new failure mode the check should catch needs an e2e scenario that shows it.
4. Keep the safety model intact: a restore check must never be able to write to
   the archive it reads ([docs/design.md](docs/design.md#safety)).

## Sign your commits (DCO)

Every commit needs a `Signed-off-by` line, certifying the
[Developer Certificate of Origin](https://developercertificate.org/). Git adds
it for you:

```sh
git commit -s -m "Describe the change"
```

The DCO check in CI fails pull requests with unsigned commits. To fix a branch:
`git rebase --signoff main` and force-push.
