# monitor-hub

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
