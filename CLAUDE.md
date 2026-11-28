# monitor-hub

<!-- prerelease:start -->
<!-- prerelease:remove-when: monitor-hub deployed on the owner's server with every monitor in monitors.json (diario, sei, anhanguera) running and working -->
## Not deployed yet

The hub does not run on the server yet: the runbook in `docs/deploy.md` has not been carried out, so no hub database holds real data and no deployed monitor depends on the current protocol. Large rewrites and deletions are fine. Change `sql/schema/00001_hub.sql` in place instead of adding a `00002` goose migration, and recreate the dev hub database afterwards; the hub applies the schema when it starts. Change the API, `proto/monitorhub`, the manifest and `settings.json` formats, `sdk-go` and `sdk-rust` outright, and keep `protocol_version` and `settings_version` at a single accepted value instead of reading old and new side by side. The monitors pin the Rust SDK by tag (`sdk-rust/v0.5.0`), so they move to the new tag when they need it. Leave no compatibility layers, don't give up on a change because it breaks dev data, and pick the best solution, not the one that breaks the least. The monitors' own databases are not covered: the old gazette and SEI services already hold real data there.

**Remove this section once the hub runs on the owner's server with diario, sei and anhanguera all working.** From then on `00001_hub.sql` no longer changes, every schema change goes into a new numbered migration, and protocol or settings format changes must keep the deployed monitors working.
<!-- prerelease:end -->

## Vocabulary

A monitor is a process the hub starts to watch something: `diario`, `sei` and `anhanguera` are monitors. Code, logs, the API and the docs call it a monitor.

"Plugin" is kept for two things. One is real plugins, planned for later, such as WhatsApp features and dependencies between monitors and plugins. The other is names that are not ours: HashiCorp go-plugin's own (`plugin.Serve`, `plugin.ClientConfig`, `plugin.GRPCController`, the `PLUGIN_*` variables, the health service name `plugin`) and protoc and buf terms, such as a protoc plugin or the `plugins:` key of `buf.gen.yaml`.

A person, also called a user, has a login and a password and signs in to a session. An admin is a person with the admin mark and can do everything. A permission is a slug, `monitor:<name>:<action>`, with `*` in place of the name for every monitor; the actions are `view`, `check`, `hook` and `configure`. A token is a personal token, `mht_`, sent as `Bearer`, or as `?token=` on the feed. A credential is a username with an `mhk_` password, sent as Basic, and it serves only hooks. A consumer's token, `mhc_`, is something else: it belongs to a consumer, not to a person.

## Where code lives

The owner's rule: "Code used by a monitor or by tools published in the SDK, such as the conformance command, lives in the SDK. Code only the hub uses lives in the hub."

- The go-plugin service set (`sdk.MonitorSet()`) and the gRPC client live in `sdk-go`, because `monitor-conformance`, which the SDK publishes, uses both and cannot import `src/internal`.
- The fake monitor of the hub's tests, `src/internal/testmonitor`, lives in the hub.
- A notifier for Go monitors, once someone writes one, goes in the SDK.

## Comments

Keep them short, and write one only where something is worth explaining. Leave obvious code without one, and never write a comment that repeats the code.
