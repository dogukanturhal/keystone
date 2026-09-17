# API Reference

## Packages
- [keystone.hexxlock.io/v1alpha1](#keystonehexxlockiov1alpha1)


## keystone.hexxlock.io/v1alpha1

Package v1alpha1 contains the first iteration of the Keystone API surface.

Stability contract for v1alpha1: any field marked with the
`+keystone:stability=alpha` marker may change between minor releases without
a deprecation cycle. Fields without that marker are intended to graduate to
v1beta1 unchanged. New fields land here first.


### Resource Types
- [AuditEntry](#auditentry)
- [AuditEntryList](#auditentrylist)
- [AuditLog](#auditlog)
- [AuditLogList](#auditloglist)
- [ClusterRegistration](#clusterregistration)
- [ClusterRegistrationList](#clusterregistrationlist)
- [DatabaseProvider](#databaseprovider)
- [DatabaseProviderList](#databaseproviderlist)
- [DatabaseSchema](#databaseschema)
- [DatabaseSchemaList](#databaseschemalist)
- [DriftReport](#driftreport)
- [DriftReportList](#driftreportlist)
- [LogicalDatabase](#logicaldatabase)
- [LogicalDatabaseList](#logicaldatabaselist)
- [MigrationBundle](#migrationbundle)
- [MigrationBundleList](#migrationbundlelist)
- [MigrationExecution](#migrationexecution)
- [MigrationExecutionList](#migrationexecutionlist)
- [MigrationPlan](#migrationplan)
- [MigrationPlanList](#migrationplanlist)
- [ProductDefinition](#productdefinition)
- [ProductDefinitionList](#productdefinitionlist)
- [ProductInstance](#productinstance)
- [ProductInstanceList](#productinstancelist)
- [RolloutPolicy](#rolloutpolicy)
- [RolloutPolicyList](#rolloutpolicylist)
- [SchemaDefinition](#schemadefinition)
- [SchemaDefinitionList](#schemadefinitionlist)
- [SchemaPolicy](#schemapolicy)
- [SchemaPolicyList](#schemapolicylist)
- [SchemaSnapshot](#schemasnapshot)
- [SchemaSnapshotList](#schemasnapshotlist)



#### AddColumnOp



AddColumnOp adds a column.



_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the new column. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `type` _string_ | Type is the PostgreSQL type (TEXT, INTEGER, NUMERIC(10,2), …).<br />Validated only at API level by length; controller hands it through<br />after identifier-style sanity checks (no semicolons or quotes). |  | MaxLength: 128 <br />Required: \{\} <br /> |
| `default` _string_ | Default is the column DEFAULT clause. Applied at column creation<br />time, so existing rows get the default. Empty = no default. |  | MaxLength: 512 <br /> |
| `nullable` _boolean_ | Nullable controls whether the column accepts NULL. Default true<br />to keep the Expand phase non-blocking; flip to false in Contract<br />phase only after backfill is confirmed. | true |  |
| `enforceNotNullInContract` _boolean_ | EnforceNotNullInContract — when true and Nullable is true, the<br />Contract phase issues SET NOT NULL after the operator confirms<br />backfill via the complete annotation. Default false. |  |  |


#### AddConstraintOp



AddConstraintOp adds a CHECK or FOREIGN KEY without locking. Other
constraint kinds (UNIQUE, PRIMARY KEY) require a different shape
(CREATE INDEX CONCURRENTLY first, then ADD CONSTRAINT USING INDEX);
admission webhooks should refuse those for now.



_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the constraint. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `type` _string_ | Type discriminates the constraint shape. |  | Enum: [check foreign_key] <br />Required: \{\} <br /> |
| `definition` _string_ | Definition is the constraint body inserted between CONSTRAINT<br />and NOT VALID. For check: the predicate, e.g. "(score >= 0 AND<br />score <= 100)". For foreign_key: the full FK clause, e.g.<br />"FOREIGN KEY (lead_id) REFERENCES leads(id)". |  | MaxLength: 2048 <br />Required: \{\} <br /> |


#### AlterColumnTypeOp



AlterColumnTypeOp changes a column's type via shadow column.



_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `column` _string_ | Column to retype. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `newType` _string_ | NewType is the target PostgreSQL type. Same charset restrictions<br />as AddColumnOp.Type. |  | MaxLength: 128 <br />Required: \{\} <br /> |
| `castExpression` _string_ | CastExpression is the SQL fragment used to convert the existing<br />value to NewType. Default: "<column>::<newType>" (PostgreSQL's<br />implicit cast). Specify a custom expression for non-trivial<br />conversions, e.g. "to_jsonb(<column>)". |  | MaxLength: 512 <br /> |


#### AppliedMigrationRecord



AppliedMigrationRecord is one row captured from the target's
schema_migrations table. Shape matches
`internal/migration.AppliedMigration` to keep the reconciler's
translation trivial.



_Appears in:_
- [SchemaSnapshotStatus](#schemasnapshotstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `version` _string_ | Version is the migration version as recorded in the tracking<br />table. Preserved verbatim — stringified even for legacy integer-<br />shaped tables. |  | MaxLength: 128 <br />Required: \{\} <br /> |
| `contentHash` _string_ | ContentHash is the SHA-256 hex over the migration's resolved<br />source at apply time. Empty string for entries from legacy<br />tables (e.g. golang-migrate's shape) that didn't record it. |  | MaxLength: 64 <br /> |
| `appliedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | AppliedAt is the timestamp the tracking table carries. |  |  |


#### AppliedStatement



AppliedStatement records what actually ran on the target schema.



_Appears in:_
- [MigrationExecutionStatus](#migrationexecutionstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `file` _string_ | File the statement was sourced from. |  |  |
| `index` _integer_ | Index within File. |  |  |
| `durationMS` _integer_ | DurationMS is the wall-clock duration in milliseconds. |  | Minimum: 0 <br /> |
| `rowsAffected` _integer_ | RowsAffected is the PostgreSQL command tag's RowsAffected. -1 for<br />statements that don't return one (most DDL). |  | Minimum: -1 <br /> |


#### ApprovalCondition



ApprovalCondition is the conditional gate on an ApprovalPolicy.
A policy fires when ALL fields here match.



_Appears in:_
- [ApprovalPolicy](#approvalpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `bundleSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#labelselector-v1-meta)_ | BundleSelector is a label selector on the MigrationBundle. An<br />empty selector matches every bundle. |  |  |
| `minLintSeverity` _[LintLevel](#lintlevel)_ | MinLintSeverity, if set, fires the policy only when the bundle<br />has at least one lint finding at or above this severity. Useful<br />for "needs senior review when there's any warning." |  | Enum: [error warning notice] <br /> |
| `riskTier` _string_ | RiskTier names the risk category the bundle must carry to<br />trigger this policy. Read from the annotation<br />`keystone.hexxlock.io/risk-tier` on the MigrationBundle. Typical<br />values: low, medium, high, critical. Empty = any tier. |  | MaxLength: 32 <br />Pattern: `^[a-z]([-a-z0-9]*[a-z0-9])?$` <br /> |


#### ApprovalPolicy



ApprovalPolicy is one row in a SchemaPolicy's N-of-M approval
workflow. Modelled after PCI DSS Req 6.5 / SOC 2 CC8 change-
management segregation-of-duties: an author cannot approve their
own change; approvals must come from named identity groups; the
apiserver records the approver's identity (see AuditEntry).

Conditional application lets operators declare, e.g.:
  "production tier bundles that touch the 'users' schema need 2
   approvals from group 'data-platform-seniors'"
  "any bundle with ≥1 warning lint finding needs 1 approval from
   group 'schema-reviewers'"



_Appears in:_
- [SchemaPolicySpec](#schemapolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the stable identifier used in the annotation-key pattern<br />`keystone.hexxlock.io/approval.<name>.<approver>=<token>`. |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `requiredApprovers` _integer_ | RequiredApprovers is the count of distinct approver identities<br />required to satisfy this policy. One identity cannot count<br />twice regardless of which groups they belong to. |  | Maximum: 16 <br />Minimum: 1 <br />Required: \{\} <br /> |
| `fromGroups` _string array_ | FromGroups is the allowed identity groups. An approver's<br />membership in any listed group satisfies the "from" clause.<br />Enforced by the approval-issuer webhook — the admission webhook<br />accepts the signed token without re-checking group membership<br />(JWT carries the subject + group claims at issuance time). |  | MaxItems: 16 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `appliesWhen` _[ApprovalCondition](#approvalcondition)_ | AppliesWhen is the conditional gate that decides whether this<br />policy fires against a given bundle. Default (empty) = always. |  |  |
| `disallowSelfApproval` _boolean_ | DisallowSelfApproval requires at least one approver to differ<br />from the bundle's author (recorded in metadata.annotations<br />under `keystone.hexxlock.io/author`). Default true — mandatory<br />separation of duties per SOC 2 CC8. Set to false only for<br />non-production tiers with documented compensating controls. | true |  |


#### ApprovalSummary



ApprovalSummary is the observable outcome of the N-of-M approval
evaluation. The overall Satisfied flag mirrors
ConditionTypeApproved; Policies carries per-ApprovalPolicy detail.



_Appears in:_
- [MigrationBundleStatus](#migrationbundlestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `satisfied` _boolean_ | Satisfied is true when every applicable ApprovalPolicy has<br />received its required approver count. False when any policy<br />fires and is under-quorum. True when no policy applies (the<br />approval gate did not block). |  | Required: \{\} <br /> |
| `policies` _[PolicyApprovalState](#policyapprovalstate) array_ | Policies is the per-(SchemaPolicy, ApprovalPolicy) breakdown,<br />sorted lexicographically for deterministic diffs. Entries for<br />policies that didn't fire (AppliesWhen=false) appear with<br />Applied=false so operators see the full rule set that was<br />considered. |  | MaxItems: 64 <br /> |


#### AuditActor



AuditActor identifies the entity responsible for the change. Populated
from Kubernetes admission request userInfo for webhook-sourced entries
and from the manager's identity for reconcile-sourced entries.



_Appears in:_
- [AuditEntrySpec](#auditentryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `username` _string_ | Username is the authenticated user's name.<br />For reconciler-sourced entries, this is<br />"system:serviceaccount:<ns>:<sa>". |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `uid` _string_ | UID is the Kubernetes-assigned UID for the actor. Stable across<br />renames; preferable to Username for correlation. |  | MaxLength: 128 <br /> |
| `groups` _string array_ | Groups are the authenticated user's groups. |  | MaxItems: 64 <br /> |


#### AuditArchiveCredsRef



AuditArchiveCredsRef points at the Secret holding S3 credentials.



_Appears in:_
- [AuditArchiveS3Spec](#auditarchives3spec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretName` _string_ | SecretName is the Secret name in the controller's system namespace. |  | MaxLength: 253 <br />MinLength: 1 <br /> |
| `accessKeyIDKey` _string_ | AccessKeyIDKey is the key in the Secret holding the S3 access<br />key ID. Default "accessKeyID". | accessKeyID |  |
| `secretAccessKeyKey` _string_ | SecretAccessKeyKey is the key in the Secret holding the S3<br />secret access key. Default "secretAccessKey". | secretAccessKey |  |


#### AuditArchiveS3Spec



AuditArchiveS3Spec is the S3-compatible endpoint configuration.
MinIO is the canonical on-prem deployment.



_Appears in:_
- [AuditArchiveSpec](#auditarchivespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | Endpoint is the S3-compatible API endpoint without scheme,<br />e.g. "minio.minio-system.svc.cluster.local:9000". |  | MaxLength: 512 <br />MinLength: 3 <br /> |
| `bucket` _string_ | Bucket is the destination bucket name. The controller does NOT<br />create the bucket; an operator-managed Crossplane S3Bucket (or<br />equivalent) must provision it with object-lock + WORM retention<br />covering RetentionDays before the controller is enabled. |  | MaxLength: 63 <br />MinLength: 3 <br />Pattern: `^[a-z0-9][a-z0-9.-]*[a-z0-9]$` <br /> |
| `region` _string_ | Region is the S3 region. MinIO accepts any value; "us-east-1"<br />is the conventional default for non-AWS S3 deployments. | us-east-1 |  |
| `useTLS` _boolean_ | UseTLS controls https vs http scheme on the endpoint URL.<br />Default true. In-cluster MinIO with mTLS via Linkerd may set<br />this to false (Linkerd handles transport security). | true |  |
| `credentialsRef` _[AuditArchiveCredsRef](#auditarchivecredsref)_ | CredentialsRef references a Secret in the controller's system<br />namespace containing the S3 access key + secret key. Keys<br />"accessKeyID" and "secretAccessKey" by default; override via<br />AccessKeyIDKey and SecretAccessKeyKey. |  | Required: \{\} <br /> |
| `pathPrefix` _string_ | PathPrefix is prepended to every archive object key. Useful<br />when one bucket carries archives from multiple clusters.<br />Default "" (objects land at the bucket root). |  | MaxLength: 256 <br /> |


#### AuditArchiveSpec



AuditArchiveSpec configures the off-cluster archive backend used
by the retention controller. Currently only S3-compatible (MinIO,
AWS S3) is implemented; future backends (Azure Blob, GCS) would
add new variants alongside the s3 field.



_Appears in:_
- [AuditLogSpec](#auditlogspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `backend` _string_ | Backend selects the archive implementation. Only "s3" is<br />supported today. "noop" forces the dormant path even when this<br />block is otherwise populated — useful for staging soak. | s3 | Enum: [s3 noop] <br /> |
| `s3` _[AuditArchiveS3Spec](#auditarchives3spec)_ | S3 holds the S3-compatible endpoint configuration.<br />Required when Backend = "s3". |  |  |
| `checkInterval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#duration-v1-meta)_ | CheckInterval controls how often the retention loop re-scans<br />AuditEntries for expiry. Default 1h. Lower for tests, higher<br />for very large clusters where the list cost is non-trivial. | 1h |  |
| `batchSize` _integer_ | BatchSize caps the number of expired entries archived in a<br />single archive object. Defaults to 1000 — high enough to keep<br />the per-object overhead low, low enough that one archive batch<br />fits comfortably in the manager Pod's memory limit (128Mi<br />requested by chart default; an entry averages ~4-6KB). | 1000 | Maximum: 10000 <br />Minimum: 10 <br /> |
| `deleteRateLimit` _integer_ | DeleteRateLimit caps AuditEntry CR deletes per second to<br />avoid the 2026-04-21-style etcd write-amplification pattern<br />where a sudden flood of writes wedges the cluster. Default 100<br />matches the watermark observed during steady-state recovery in<br />that incident. | 100 | Maximum: 10000 <br />Minimum: 1 <br /> |


#### AuditEntry



AuditEntry is one immutable record in Keystone's append-only audit
chain. See the package doc of `internal/audit/` for the verifier
algorithm and the integration guide in
`docs/compliance-mappings.md`.



_Appears in:_
- [AuditEntryList](#auditentrylist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `AuditEntry` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[AuditEntrySpec](#auditentryspec)_ |  |  |  |
| `status` _[AuditEntryStatus](#auditentrystatus)_ |  |  |  |


#### AuditEntryList



AuditEntryList is the list wrapper.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `AuditEntryList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[AuditEntry](#auditentry) array_ |  |  |  |


#### AuditEntrySpec



AuditEntrySpec is one immutable entry in Keystone's append-only
audit log. Entries are chained via SHA-256 hashes so downstream
verifiers can detect tampering without trusting the apiserver.

Invariant enforced by the admission webhook
(`internal/webhook/auditentry_webhook.go`): once created, every
field in this struct is frozen. Updates and deletes are rejected.

Entries are cluster-scoped and ordered by `sequence`. A singleton
`AuditLog` resource holds the next sequence number and serves as
the optimistic-concurrency token for concurrent writers.

Map to compliance controls:
  - SOC 2 CC7.2 + CC8 change management
  - ISO 27001 Annex A 8.15 logging + 8.16 monitoring + 8.32 change mgmt
  - HIPAA §164.312(b) audit controls
  - PCI DSS v4.0.1 Requirement 10



_Appears in:_
- [AuditEntry](#auditentry)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sequence` _integer_ | Sequence is the monotonic cluster-wide entry number. Assigned by<br />the AuditLogger from the singleton AuditLog's status.nextSequence.<br />Gaps are not permitted in a compliant chain — verifier tooling<br />MUST reject a log with missing sequence numbers. |  | Minimum: 1 <br />Required: \{\} <br /> |
| `timestamp` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | Timestamp is when the audited action took place. Not necessarily<br />the same as the AuditEntry's metadata.creationTimestamp — there<br />can be small delays between the action and the entry being<br />persisted. |  | Required: \{\} <br /> |
| `actor` _[AuditActor](#auditactor)_ | Actor identifies who or what caused the state change. |  | Required: \{\} <br /> |
| `verb` _string_ | Verb is the class of action audited. |  | Enum: [create update delete reconcile approve deny drift-ack rollout-pause rollout-resume apply] <br />Required: \{\} <br /> |
| `resourceRef` _[AuditResourceRef](#auditresourceref)_ | ResourceRef points at the object the action concerned. |  | Required: \{\} <br /> |
| `before` _string_ | Before is the JSON-encoded state of the resource prior to the<br />action. Empty for verb=create. |  | MaxLength: 65536 <br /> |
| `after` _string_ | After is the JSON-encoded state of the resource after the action.<br />Empty for verb=delete. |  | MaxLength: 65536 <br /> |
| `reason` _string_ | Reason is a human-readable description of why the action was<br />taken. For reconcile verbs, this is typically the condition<br />reason (e.g. "Reconciled", "LintFailed"). For approve/deny, the<br />approver's note. |  | MaxLength: 1024 <br /> |
| `outcome` _string_ | Outcome records success or failure. |  | Enum: [success error warn] <br />Required: \{\} <br /> |
| `prevHash` _string_ | PrevHash is the SHA-256 (hex) of the previous entry's SelfHash.<br />The first entry in the chain carries the literal string<br />"genesis" — this is how verifiers detect the chain's origin. |  | MaxLength: 96 <br />Pattern: `^(genesis\|[0-9a-f]\{64\})$` <br />Required: \{\} <br /> |
| `selfHash` _string_ | SelfHash is the SHA-256 (hex) over a canonical encoding of this<br />entry's fields EXCLUDING SelfHash itself. The admission webhook<br />verifies this matches on every create — entries that don't hash<br />to their claimed SelfHash are rejected outright. |  | MaxLength: 64 <br />Pattern: `^[0-9a-f]\{64\}$` <br />Required: \{\} <br /> |


#### AuditEntryStatus



AuditEntryStatus is intentionally minimal. All load-bearing state
lives in spec (which is immutable). Status-only fields can change
without breaking integrity.



_Appears in:_
- [AuditEntry](#auditentry)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `verifiedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | VerifiedAt is when a verifier tool last validated this entry's<br />hash against the preceding entry. Optional — external SIEMs<br />typically carry their own verification receipts. |  |  |
| `verifiedBy` _string_ | VerifiedBy names the verifier tool. |  | MaxLength: 256 <br /> |


#### AuditLog



AuditLog is the singleton coordination CR for the append-only
audit chain. Exactly one instance named `AuditLogName` ("keystone")
exists per cluster.



_Appears in:_
- [AuditLogList](#auditloglist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `AuditLog` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[AuditLogSpec](#auditlogspec)_ |  |  |  |
| `status` _[AuditLogStatus](#auditlogstatus)_ |  |  |  |


#### AuditLogList



AuditLogList is the list wrapper.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `AuditLogList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[AuditLog](#auditlog) array_ |  |  |  |


#### AuditLogSpec



AuditLogSpec is static configuration for the audit chain.



_Appears in:_
- [AuditLog](#auditlog)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `retentionDays` _integer_ | RetentionDays is the minimum retention for AuditEntries before a<br />future cleanup job (Phase 12.3) may prune them. Default 2555 =<br />~7 years, which covers SOC 2 / PCI / HIPAA retention floors. | 2555 | Maximum: 36500 <br />Minimum: 90 <br /> |
| `exportEndpoint` _string_ | ExportEndpoint optionally declares a SIEM / log-aggregator the<br />manager streams entries to. Format:<br />  "otlp-http://collector:4318"<br />  "kafka://broker:9092/audit-topic"<br />  "webhook://https://siem.example.com/ingest"<br />Empty disables external streaming — entries still land in etcd. |  | MaxLength: 2048 <br /> |
| `archive` _[AuditArchiveSpec](#auditarchivespec)_ | Archive optionally enables the AuditEntry retention controller.<br />When set, entries older than RetentionDays are batch-uploaded<br />to the configured S3-compatible bucket as gzipped JSON Lines and<br />then deleted from etcd, freeing the cluster from unbounded<br />chain growth. When unset (default), the retention controller<br />stays dormant — entries grow forever.<br />The archive backend MUST support object-lock compliance mode<br />(S3 retention + WORM) for tier-1 compliance: even with archive<br />enabled, deleted CRs must remain immutable for the full<br />retention window in the off-cluster store. |  |  |


#### AuditLogStatus



AuditLogStatus carries the cluster-wide monotonic sequence counter.
Optimistic-concurrency updates on this subresource are what serialise
audit writes across manager replicas.



_Appears in:_
- [AuditLog](#auditlog)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `nextSequence` _integer_ | NextSequence is the sequence number the next AuditEntry will be<br />assigned. Starts at 1. |  |  |
| `lastSequence` _integer_ | LastSequence is the highest sequence number that has been<br />successfully persisted. Equals NextSequence-1 outside write<br />windows. Surfaced for observability dashboards. |  |  |
| `lastEntryHash` _string_ | LastEntryHash is the SelfHash of the most recently persisted<br />AuditEntry. Writers use this as the PrevHash input for the next<br />entry, so the chain stays linked across restarts. |  | MaxLength: 64 <br /> |
| `lastEntryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastEntryTime is when the most recent entry was persisted. Feeds<br />the `keystone_audit_last_entry_age_seconds` metric which should<br />alert if the chain goes quiet unexpectedly. |  |  |
| `lastArchivedSequence` _integer_ | LastArchivedSequence is the highest sequence number that has<br />been successfully archived (uploaded to off-cluster storage AND<br />deleted from etcd) by the retention controller. Zero when no<br />entries have ever been archived. Always <= LastSequence. |  |  |
| `lastArchivedTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastArchivedTime records the timestamp of the most recent<br />successful archive batch. Feeds the<br />`keystone_audit_archive_lag_seconds` gauge — alert if this<br />stops advancing past RetentionDays + 1d. |  |  |
| `lastArchivedHash` _string_ | LastArchivedHash is the SelfHash of the highest-sequence<br />AuditEntry in the most recent archive batch. Feeds chain<br />integrity verification: when re-hydrating archived entries,<br />the operator can confirm the genesis-to-LastArchivedHash chain<br />remains intact end-to-end. |  | MaxLength: 64 <br /> |


#### AuditResourceRef



AuditResourceRef locates the audited object.



_Appears in:_
- [AuditEntrySpec](#auditentryspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | APIVersion of the referenced object (e.g.<br />"keystone.hexxlock.io/v1alpha1"). |  | MaxLength: 128 <br />Required: \{\} <br /> |
| `kind` _string_ | Kind of the referenced object (e.g. "MigrationBundle"). |  | MaxLength: 128 <br />Required: \{\} <br /> |
| `namespace` _string_ | Namespace of the referenced object. Empty for cluster-scoped<br />objects. |  | MaxLength: 253 <br /> |
| `name` _string_ | Name of the referenced object. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `uid` _string_ | UID of the referenced object at the time of audit. |  | MaxLength: 128 <br /> |


#### CELRule



CELRule is one Common Expression Language rule evaluated at
admission time. Expressions return a bool — true means "this rule
fires". Severity controls whether the fire blocks admission
(Error) or surfaces as a warning (Warning).

Activation variables available to expressions:

  - bundle:       MigrationBundle fields (metadata, spec)
  - findings:     []LintFinding from the analyzer pass
  - strategy:     shortcut for bundle.spec.strategy
  - labels:       shortcut for bundle.metadata.labels (map)
  - annotations:  shortcut for bundle.metadata.annotations (map)
  - version:      shortcut for bundle.spec.version

Example expressions:

  - `strategy == "versioned" && size(bundle.spec.operations) > 0`
  - `labels["tier"] == "prod" && findings.exists(f, f.severity == "warning")`
  - `!("keystone.hexxlock.io/jira" in annotations)`



_Appears in:_
- [SchemaPolicySpec](#schemapolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the rule's stable identifier. Surfaced in rejection<br />messages and warning output. Required. |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `expression` _string_ | Expression is the CEL source. Must compile against Keystone's<br />activation variables and return a boolean. Rules that fail to<br />compile produce a ConfigurationError at admission — they do<br />NOT silently disable. |  | MaxLength: 4096 <br />Required: \{\} <br /> |
| `severity` _[CELSeverity](#celseverity)_ | Severity selects the admission action when the expression<br />evaluates true:<br />  - Warning: appended to admission warnings; bundle admits.<br />  - Error:   rejects admission with Message + expression text.<br />Defaults to Error (strictest outcome) so forgetting severity<br />fails loud. | Error | Enum: [Warning Error] <br /> |
| `message` _string_ | Message is the human-readable rejection/warning text. Keep it<br />specific — operators reading admission output want to know<br />what's wrong and how to fix. Leave empty for a generic<br />"<rule-name> fired" message. |  | MaxLength: 1024 <br /> |


#### CELSeverity

_Underlying type:_ _string_

CELSeverity is the admission action taken when a rule fires.

_Validation:_
- Enum: [Warning Error]

_Appears in:_
- [CELRule](#celrule)

| Field | Description |
| --- | --- |
| `Warning` | CELSeverityWarning surfaces the rule as an admission warning<br />(kubectl prints it above the accepted/rejected line) but does<br />not block admission.<br /> |
| `Error` | CELSeverityError blocks admission with the rule's message.<br /> |


#### CleanupPolicy



CleanupPolicy is the per-SchemaDefinition retention configuration for
terminal MigrationBundle + MigrationExecution CRs. Mirrors Argo
Workflows' spec.ttlStrategy.{secondsAfterSuccess, secondsAfterFailure}
shape but uses metav1.Duration so operators write ergonomic strings
like "24h" and "7d" rather than raw integer seconds.

Both fields are optional — leaving either unset falls through to the
operator-wide defaults (RetainSuccessFor=24 h, RetainFailureFor=7 d).
A zero Duration is rejected: it would mean "delete immediately on
terminal phase observation", which removes the operator's window to
emit metrics and Events about the success/failure outcome. Negative
durations are rejected at admission via the Minimum kubebuilder
marker.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `retainSuccessFor` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#duration-v1-meta)_ | RetainSuccessFor is how long to keep a Succeeded MigrationBundle<br />or MigrationExecution before garbage collection. The latest<br />Succeeded CR per kind is ALWAYS retained regardless of age — the<br />SD reconciler reads it for re-emit decisions and the operator<br />uses it to answer "what last shipped". This field controls the<br />retention window for older Succeeded siblings, where one<br />generation per fan-out tenant accumulates at every reconcile.<br />Operators wanting longer post-mortem windows on success can bump<br />this to 7d or more. The 2026-05-08 outage post-mortem suggests<br />24 h is the right default: long enough for humans to see the<br />rollout in `kubectl get mb -A`, short enough that a 100-tenant<br />fan-out reconciling every 30 min doesn't accumulate beyond<br />O(thousands).<br />Empty / nil = use the operator-wide default (24h). |  |  |
| `retainFailureFor` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#duration-v1-meta)_ | RetainFailureFor is how long to keep a Failed / Aborted /<br />RollbackFailed MigrationBundle or MigrationExecution before<br />garbage collection. Failures need a longer post-mortem window<br />than successes — the on-call engineer who picks up the page on<br />Friday afternoon needs the failure CR to still exist Monday<br />morning. 7d default matches the standard "two weekend windows"<br />rule.<br />Empty / nil = use the operator-wide default (7d). |  |  |


#### ClusterRegistration



ClusterRegistration registers a workload cluster with the Keystone hub.
Cluster-scoped because clusters are not a tenant resource.



_Appears in:_
- [ClusterRegistrationList](#clusterregistrationlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `ClusterRegistration` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterRegistrationSpec](#clusterregistrationspec)_ |  |  |  |
| `status` _[ClusterRegistrationStatus](#clusterregistrationstatus)_ |  |  |  |


#### ClusterRegistrationList



ClusterRegistrationList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `ClusterRegistrationList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[ClusterRegistration](#clusterregistration) array_ |  |  |  |


#### ClusterRegistrationSpec



ClusterRegistrationSpec describes a workload cluster that the hub may
target. The agent running inside the cluster reconciles its own local
CRs; the hub uses this record to plan multi-cluster rollouts and to
enumerate targets for ApplicationSet generators.

Spec is intentionally small. Cluster connectivity (kubeconfig, server URL,
CA bundle) is owned by ArgoCD's cluster secrets — Keystone deliberately
does not duplicate that state.



_Appears in:_
- [ClusterRegistration](#clusterregistration)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `displayName` _string_ | DisplayName is a human-readable label shown in dashboards and audit<br />logs. Defaults to the resource name when empty. |  | MaxLength: 253 <br /> |
| `tier` _[ClusterTier](#clustertier)_ | Tier classifies the cluster for rollout staging. Required: there is<br />no safe default — operators must consciously place a cluster. |  | Enum: [canary free paid internal] <br />Required: \{\} <br /> |
| `region` _string_ | Region is the geographic region this cluster lives in (e.g.<br />"eu-west-1", "us-east-1"). Used by RolloutPolicy region selectors and<br />by audit reporting. |  | MaxLength: 63 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `dataResidency` _string_ | DataResidency is the legal jurisdiction the cluster's data is<br />permitted to live in (e.g. "eu", "us", "global"). Distinct from<br />Region: a cluster can be in eu-west-1 but bound by EU residency rules.<br />Required so that residency-tagged workloads cannot accidentally land<br />in a non-compliant cluster. |  | MaxLength: 16 <br />Pattern: `^[a-z0-9-]+$` <br />Required: \{\} <br /> |
| `labels` _object (keys:string, values:string)_ | Labels are user-defined classifiers consumed by RolloutPolicy and<br />MigrationBundle target selectors. Distinct from metadata.labels:<br />metadata.labels are shared with kubectl and other tooling; spec.labels<br />are explicitly part of Keystone's API surface and form a stable<br />selector contract. |  | MaxProperties: 64 <br /> |


#### ClusterRegistrationStatus



ClusterRegistrationStatus reports observed runtime state. Only the
controller writes Status; users must not edit it.



_Appears in:_
- [ClusterRegistration](#clusterregistration)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled.<br />ArgoCD compares this against .metadata.generation to determine<br />whether the controller has seen the latest spec. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state. ConditionTypeReady is always<br />present; controllers may add more (e.g. "Connected"). |  |  |
| `lastHeartbeatTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastHeartbeatTime is the last time the agent in this cluster reported<br />in. Populated by the agent posting back through Git status. A stale<br />heartbeat (older than the agent's reporting interval × 3) is grounds<br />for marking the cluster Degraded. |  |  |
| `agentVersion` _string_ | AgentVersion is the version of keystone-manager running in agent<br />mode in this cluster. Used to gate features that require a minimum<br />agent version. |  | MaxLength: 64 <br /> |


#### ClusterTier

_Underlying type:_ _string_

ClusterTier classifies a workload cluster for staged rollout. The
RolloutController honours these tiers when fanning out a MigrationPlan:
canary first, then free in batches, then paid with low parallelism.

_Validation:_
- Enum: [canary free paid internal]

_Appears in:_
- [ClusterRegistrationSpec](#clusterregistrationspec)
- [RolloutStage](#rolloutstage)

| Field | Description |
| --- | --- |
| `canary` | ClusterTierCanary receives every change first. Used to catch breakage<br />before any tenant traffic is exposed.<br /> |
| `free` | ClusterTierFree receives changes in moderate-parallelism batches once<br />canary has been observed healthy.<br /> |
| `paid` | ClusterTierPaid receives changes last, with low parallelism and an<br />optional approval gate set on the RolloutPolicy.<br /> |
| `internal` | ClusterTierInternal hosts platform-internal workloads (control plane,<br />observability, CI). Migrations here are scheduled independently of the<br />canary-free-paid pipeline.<br /> |


#### ColumnType

_Underlying type:_ _string_

ColumnType is the allowed set of column types. Restricted from "any
string" to a whitelist so the API can't be abused to inject DDL via
the type field. New types land via API review.

_Validation:_
- MaxLength: 128
- Pattern: `^[a-z][a-z0-9_ (),\[\]]*$`

_Appears in:_
- [DesiredColumn](#desiredcolumn)



#### DatabaseProvider



DatabaseProvider declares a physical database cluster the operator may
administer. LogicalDatabase resources reference a DatabaseProvider via
spec.providerRef.



_Appears in:_
- [DatabaseProviderList](#databaseproviderlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `DatabaseProvider` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[DatabaseProviderSpec](#databaseproviderspec)_ |  |  |  |
| `status` _[DatabaseProviderStatus](#databaseproviderstatus)_ |  |  |  |


#### DatabaseProviderCredentials



DatabaseProviderCredentials references the Secret holding admin
credentials for the provider. The Secret MUST live in the
keystone-system namespace; cross-namespace references are rejected to
avoid privilege escalation through Secret accessibility.



_Appears in:_
- [DatabaseProviderSpec](#databaseproviderspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `secretName` _string_ | SecretName is the name of the Secret in the keystone-system<br />namespace. Required. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `usernameKey` _string_ | UsernameKey is the Secret data key for the admin username. | username | MaxLength: 253 <br /> |
| `passwordKey` _string_ | PasswordKey is the Secret data key for the admin password. | password | MaxLength: 253 <br /> |


#### DatabaseProviderEngine

_Underlying type:_ _string_

DatabaseProviderEngine identifies the database engine. Phase 2 supports
PostgreSQL only; the enum exists so engine-pluggability lands without an
API break.

_Validation:_
- Enum: [postgresql]

_Appears in:_
- [DatabaseProviderSpec](#databaseproviderspec)

| Field | Description |
| --- | --- |
| `postgresql` |  |


#### DatabaseProviderList



DatabaseProviderList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `DatabaseProviderList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[DatabaseProvider](#databaseprovider) array_ |  |  |  |


#### DatabaseProviderSSLMode

_Underlying type:_ _string_

DatabaseProviderSSLMode mirrors libpq's sslmode values. The default is
`verify-full`; `disable` is intentionally NOT permitted on the API
surface — operators that want to opt out have to write a custom OPA
policy to override the admission webhook.

_Validation:_
- Enum: [require verify-ca verify-full]

_Appears in:_
- [DatabaseProviderSpec](#databaseproviderspec)

| Field | Description |
| --- | --- |
| `require` |  |
| `verify-ca` |  |
| `verify-full` |  |


#### DatabaseProviderSpec



DatabaseProviderSpec describes a physical PostgreSQL cluster reachable
from the operator. Cluster-scoped because providers are platform
infrastructure shared across tenants and namespaces.



_Appears in:_
- [DatabaseProvider](#databaseprovider)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `engine` _[DatabaseProviderEngine](#databaseproviderengine)_ | Engine declares the database engine. PostgreSQL only in Phase 2. |  | Enum: [postgresql] <br />Required: \{\} <br /> |
| `host` _string_ | Host is the DNS name or IP of the primary writer endpoint. The<br />controller routes administrative DDL to this endpoint; read replicas<br />are out of scope for SchemaController. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `port` _integer_ | Port is the TCP port the engine listens on. | 5432 | Maximum: 65535 <br />Minimum: 1 <br /> |
| `maintenanceDatabase` _string_ | MaintenanceDatabase is the database the controller connects to for<br />administrative work that cannot run inside a target database<br />(e.g. CREATE DATABASE). Defaults to "postgres". | postgres | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br /> |
| `sslMode` _[DatabaseProviderSSLMode](#databaseprovidersslmode)_ | SSLMode controls TLS verification. `disable` is intentionally<br />unavailable; downgrade requires an out-of-band policy override. | verify-full | Enum: [require verify-ca verify-full] <br /> |
| `adminCredentialsRef` _[DatabaseProviderCredentials](#databaseprovidercredentials)_ | AdminCredentialsRef references the Secret holding admin user/password. |  | Required: \{\} <br /> |
| `poolMaxConns` _integer_ | PoolMaxConns caps the controller's pgx pool size against this<br />provider. Higher values speed up large-fanout reconciles but consume<br />more PG backends; tune per workload. | 4 | Maximum: 64 <br />Minimum: 1 <br /> |


#### DatabaseProviderStatus



DatabaseProviderStatus reports observed connectivity.



_Appears in:_
- [DatabaseProvider](#databaseprovider)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state. Standard set: Ready,<br />Available, Progressing, Degraded. Specific to providers: Connected<br />(controller has an open admin session and SELECT 1 succeeded within<br />the last reconcile interval). |  |  |
| `serverVersion` _string_ | ServerVersion is the PostgreSQL server_version reported by the<br />provider. Surfaced for ops visibility and for migration policy<br />rules that want to refuse running on outdated versions. |  | MaxLength: 64 <br /> |
| `lastConnectTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastConnectTime is when the controller last successfully opened an<br />admin connection. |  |  |
| `credentialsObservedVersion` _string_ | CredentialsObservedVersion is the ResourceVersion of the admin<br />Secret at the last DatabaseProviderReconciler observation. Used<br />to detect rotation: when the live Secret's ResourceVersion<br />differs, the reconciler triggers pool eviction so stale<br />credentials don't outlive the ESO sync. |  | MaxLength: 64 <br /> |
| `credentialsRotatedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | CredentialsRotatedAt is the timestamp the reconciler last<br />observed a change in the admin Secret's ResourceVersion. Empty<br />until the first rotation after the provider is admitted.<br />Useful for correlating rotation events with SIEM entries. |  |  |
| `observedConnectionProfile` _string_ | ObservedConnectionProfile is the "host:port" endpoint the<br />reconciler most recently observed on spec. When the spec is<br />repointed (server migration, DNS→IP cutover, local<br />port-forward verification) the reconciler compares the live<br />spec against this value, evicts cached pools still dialing the<br />previous endpoint, and updates the field. Mirrors<br />CredentialsObservedVersion, but for the CR-visible half of the<br />connection profile instead of the Secret. |  | MaxLength: 320 <br /> |


#### DatabaseSchema



DatabaseSchema declares a PostgreSQL schema that should exist inside a
referenced LogicalDatabase. The combination of LogicalDatabase +
DatabaseSchema replaces the hardcoded {SharedDB, Schema} pairs in the
legacy db-migrator.



_Appears in:_
- [DatabaseSchemaList](#databaseschemalist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `DatabaseSchema` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[DatabaseSchemaSpec](#databaseschemaspec)_ |  |  |  |
| `status` _[DatabaseSchemaStatus](#databaseschemastatus)_ |  |  |  |


#### DatabaseSchemaList



DatabaseSchemaList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `DatabaseSchemaList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[DatabaseSchema](#databaseschema) array_ |  |  |  |


#### DatabaseSchemaSpec



DatabaseSchemaSpec describes one PostgreSQL schema (`CREATE SCHEMA`).
Schemas live inside a LogicalDatabase, declared by reference. The
controller creates the schema, sets ownership, and manages default
privileges.



_Appears in:_
- [DatabaseSchema](#databaseschema)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the actual PostgreSQL schema name. Distinct from<br />metadata.name for the same reason as LogicalDatabase.spec.name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `logicalDatabaseRef` _string_ | LogicalDatabaseRef is the name of the LogicalDatabase this schema<br />lives in. Must be in the same namespace as this DatabaseSchema —<br />cross-namespace references would create an authorisation hole. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `ownerRole` _string_ | OwnerRole is the PostgreSQL role that owns the schema. The<br />controller creates the role if absent and runs `ALTER SCHEMA OWNER<br />TO`. Required for the same reasons as LogicalDatabase.spec.ownerRole. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `defaultPrivileges` _[SchemaPrivilegeDefault](#schemaprivilegedefault) array_ | DefaultPrivileges declares ALTER DEFAULT PRIVILEGES rules applied at<br />schema creation. See the CNPG-managed-roles landmine documented in<br />project memory: pre-existing schemas need GRANT / REASSIGN OWNED<br />before role hand-off, which the controller handles automatically<br />when ownership changes. |  | MaxItems: 32 <br /> |
| `searchPathHints` _string array_ | SearchPathHints is an optional list of schemas to prepend to the<br />owner role's search_path. Empty means leave the role's search_path<br />unmodified. Useful for cross-schema modules (e.g. a `crm` schema<br />that reads from `master_data`). |  | MaxItems: 16 <br /> |
| `deletionPolicy` _string_ | DeletionPolicy controls what happens to the underlying PostgreSQL<br />schema when this resource is deleted. Defaults to Retain. | Retain | Enum: [Retain Delete] <br /> |


#### DatabaseSchemaStatus



DatabaseSchemaStatus reports observed state.



_Appears in:_
- [DatabaseSchema](#databaseschema)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state. Standard set: Ready, Available,<br />Progressing, Degraded. Migration-specific conditions (Applied,<br />DriftFree) are added by the MigrationController in Phase 3. |  |  |
| `migrationVersion` _string_ | MigrationVersion is the highest schema_migrations version applied to<br />this schema. Surfaced by the MigrationController; null until the<br />first migration runs. |  |  |
| `lastAppliedFingerprint` _string_ | LastAppliedFingerprint is the SchemaDefinition.Status.Fingerprint<br />value of the most recent SD-emitted MigrationBundle that the<br />runner applied to this schema successfully. The bundle controller<br />stamps it after a MigrationExecution reaches Phase=Succeeded for<br />(this schema, the bundle carrying the SD fingerprint annotation).<br />SDK consumers (pkg/sdk/keystone.WaitSchemaFingerprint) compare<br />this field against SchemaDefinition.Status.Fingerprint to verify<br />per-schema convergence — race-free vs the matchedSchemas-count<br />baseline that WaitSchemaUpToDate (v0.1.47) uses.<br />Empty until the SD-emit-then-apply cycle has run at least once<br />for this schema. Hand-authored MigrationBundles (no SD owner)<br />don't carry the SD fingerprint annotation and won't update this<br />field; that's intentional — fingerprint convergence is an<br />SD-owned concept. |  | MaxLength: 64 <br /> |
| `lastReconcileTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastReconcileTime is when the controller last successfully reconciled<br />this resource. |  |  |


#### DesiredCheckConstraint



DesiredCheckConstraint is one table-level CHECK constraint.

Single-column CHECKs can also be expressed via DesiredColumn.Check
(when shipped as a future API addition); use this list for
multi-column predicates and named constraints that integration
tests assert by name.



_Appears in:_
- [DesiredTable](#desiredtable)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the constraint. Must be unique within the table and<br />follow the standard PG identifier rules (snake_case, max 63<br />chars, leading underscore allowed). |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `definition` _string_ | Definition is the predicate expression to enforce, exactly as<br />it would appear inside the CHECK (...) parentheses. The differ<br />does NOT canonicalise this string before comparing to<br />pg_constraint.consrc — produce it via the same expression<br />printer keystonectl uses to avoid spurious diffs. |  | MaxLength: 1024 <br />MinLength: 1 <br />Required: \{\} <br /> |


#### DesiredColumn



DesiredColumn is one column declaration.



_Appears in:_
- [DesiredTable](#desiredtable)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the column identifier. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `type` _[ColumnType](#columntype)_ | Type is the PostgreSQL data type (no default — every column must<br />declare it explicitly). |  | MaxLength: 128 <br />Pattern: `^[a-z][a-z0-9_ (),\[\]]*$` <br />Required: \{\} <br /> |
| `nullable` _boolean_ | Nullable controls NOT NULL. Default true because declaring a<br />NOT NULL column without a default on an existing populated table<br />requires a backfill dance; the differ will refuse to plan that.<br />`omitempty` was removed deliberately: keystonectl-inspect-generated<br />SchemaDefinitions emit `nullable: false` for NOT NULL columns; with<br />`omitempty` the false value would be stripped from JSON, K8s would<br />then apply the kubebuilder default of `true` at admission, and<br />every NOT NULL column would round-trip into the cluster as nullable<br />— producing thousands of bogus ALTER TABLE ops on every reconcile.<br />Always emit the field so the admission defaulter only fires when<br />the user genuinely omits it. | true |  |
| `default` _string_ | Default is the column DEFAULT clause. Empty = no default. |  | MaxLength: 512 <br /> |
| `primaryKey` _boolean_ | PrimaryKey marks the column as the table's single-column PK.<br />For composite PKs, use DesiredTable.PrimaryKey instead. |  |  |
| `identity` _string_ | Identity declares the column as a PostgreSQL identity column:<br />`GENERATED ALWAYS AS IDENTITY` or `GENERATED BY DEFAULT AS<br />IDENTITY`. Empty means an ordinary column.<br />The two spellings are not cosmetic. Under ALWAYS, PostgreSQL<br />rejects a user-supplied value unless the INSERT says OVERRIDING<br />SYSTEM VALUE; under BY DEFAULT the user's value wins. Collapsing<br />them would silently change whether an application's inserts are<br />accepted, so the distinction is carried through the desired state<br />rather than normalised away.<br />Values are the SQL keywords rather than CamelCase, matching the<br />rest of this API (ColumnType carries raw PostgreSQL type names,<br />DesiredIndex.Method carries "btree"), and matching what the<br />inspector reads back out of pg_attribute.attidentity.<br />Identity implies NOT NULL — PostgreSQL marks such columns NOT NULL<br />automatically — but it does NOT imply uniqueness: a sequence can be<br />reset or a value inserted by hand, so a PRIMARY KEY or UNIQUE<br />constraint is still required to guarantee it. |  | Enum: [ ALWAYS BY DEFAULT] <br /> |
| `generated` _string_ | Generated is the expression of a STORED generated column —<br />`GENERATED ALWAYS AS (<expr>) STORED`. Empty means an ordinary<br />column.<br />Only STORED is modelled. PostgreSQL had no other kind before 18,<br />and the inspector reads the expression back from a stored column;<br />when VIRTUAL becomes a target, it arrives as a sibling field rather<br />than by overloading this one.<br />Mutually exclusive with Default and with Identity: a generated<br />column computes its value, so a DEFAULT or an identity sequence has<br />nothing to apply to. |  | MaxLength: 2048 <br /> |


#### DesiredDefaultPrivilege



DesiredDefaultPrivilege expresses one `ALTER DEFAULT PRIVILEGES`
statement that the SD reconciler emits as part of the bundle.

Semantics:

	ALTER DEFAULT PRIVILEGES FOR ROLE <ForRole>
	  IN SCHEMA <Schema OR sd.targetSchema>
	  GRANT <Privileges> ON <ObjectType> TO <ToRole>;

Idempotent in PostgreSQL — re-issuing the same grant against the
same role/schema/object-type is a no-op (pg_default_acl uniqueness).

Practical example for the example-service 2026-05-12 fix:

	defaultPrivileges:
	  - forRole: keystone_admin
	    toRole:  falcon_id_app
	    objectType: tables
	    privileges: [SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER]
	  - forRole: keystone_admin
	    toRole:  falcon_id_app
	    objectType: sequences
	    privileges: [USAGE, SELECT, UPDATE]



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `forRole` _string_ | ForRole is the PG role whose CREATE actions the grant attaches<br />to. For Keystone-managed schemas this is typically the role the<br />operator runs migrations as — `keystone_admin`. Required. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `toRole` _string_ | ToRole is the PG role receiving the granted privileges. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `schema` _string_ | Schema names the schema the default-privileges apply IN. When<br />empty, defaults to the SchemaDefinition's target schema (i.e.<br />the schema resolved via SchemaRef or SchemaSelector). Use<br />"public" for the public schema. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br /> |
| `objectType` _string_ | ObjectType is the PG default-privileges object class. Mirrors<br />the keyword that follows ON in PostgreSQL's<br />`ALTER DEFAULT PRIVILEGES ... ON <ObjectType>` syntax. Common<br />values: tables, sequences, functions, types. |  | Enum: [tables sequences functions types schemas] <br />Required: \{\} <br /> |
| `privileges` _string array_ | Privileges is the privilege set granted. Validated against the<br />PG privilege keywords valid for the given ObjectType — e.g.<br />`tables` accepts SELECT/INSERT/UPDATE/DELETE/TRUNCATE/REFERENCES<br />/TRIGGER/MAINTAIN; `sequences` accepts USAGE/SELECT/UPDATE.<br />The literal string `ALL` is also accepted (Keystone analyzers<br />surface a warning for the GRANT-ALL footgun via the<br />`no-grant-all` rule.) |  | MaxItems: 16 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `withGrantOption` _boolean_ | WithGrantOption mirrors the SQL `WITH GRANT OPTION` clause.<br />When true the receiving role can re-grant the privileges to<br />other roles. Default false. |  |  |


#### DesiredEnum



DesiredEnum declares a PostgreSQL enum type.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the enum type name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `values` _string array_ | Values is the ordered list of enum labels. |  | MaxItems: 256 <br />MinItems: 1 <br />Required: \{\} <br /> |


#### DesiredExtension



DesiredExtension declares a PostgreSQL extension the schema depends on
(pgcrypto for gen_random_uuid(), citext, postgis, …).

Extensions are part of the desired state because they are a
prerequisite for it: a column defaulting to gen_random_uuid() fails to
create unless pgcrypto exists. Without this the differ cannot tell an
extension-provided object from drift, and re-creates or drops objects
the extension owns.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the extension name as CREATE EXTENSION spells it. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_-]*$` <br />Required: \{\} <br /> |
| `schema` _string_ | Schema pins the extension to a specific schema. Empty installs it<br />wherever the target's search_path puts it, which is the right<br />default: extensions are commonly shared across schemas, and pinning<br />one to the schema currently being diffed would make the desired<br />state non-portable between environments. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br /> |


#### DesiredForeignKey



DesiredForeignKey is one foreign key declaration.



_Appears in:_
- [DesiredTable](#desiredtable)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the constraint. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `columns` _string array_ | Columns in this table referencing the target. |  | MaxItems: 8 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `referencesTable` _string_ | ReferencesTable is the target table name (same schema as the<br />containing SchemaDefinition — cross-schema FKs are deliberately<br />out of scope for v1alpha1). |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `referencesColumns` _string array_ | ReferencesColumns is the target column list. |  | MaxItems: 8 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `onDelete` _string_ | OnDelete action — CASCADE, SET NULL, SET DEFAULT, RESTRICT, NO ACTION. | NO ACTION | Enum: [CASCADE SET NULL SET DEFAULT RESTRICT NO ACTION] <br /> |


#### DesiredFunction



DesiredFunction declares a PostgreSQL function or procedure.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the function name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `args` _string_ | Args is the argument signature (e.g., "p_id uuid, p_name text").<br />Empty string for zero-argument functions. |  | MaxLength: 2048 <br /> |
| `returns` _string_ | Returns is the return type (e.g., "void", "TABLE(id uuid, name text)"). |  | MaxLength: 2048 <br />Required: \{\} <br /> |
| `language` _string_ | Language is the PG language. | plpgsql | Enum: [plpgsql sql c internal] <br /> |
| `body` _string_ | Body is the function body (between $$ delimiters). |  | MaxLength: 65536 <br />Required: \{\} <br /> |
| `replace` _boolean_ | Replace controls CREATE OR REPLACE semantics. Default true<br />because functions are safely replaceable. | true |  |


#### DesiredIndex



DesiredIndex is one index declaration.

Three column-spec shapes are supported (use exactly one):
  1. Columns []string                 — simple; one btree col per entry,
                                         ASC default, no opclass.
  2. ColumnRefs []DesiredIndexColumn   — rich; per-column ASC/DESC,
                                         NULLS FIRST/LAST, opclass.
  3. Expression string                 — single expression-index body
                                         (e.g. "lower(email)" or
                                         "coalesce(deleted_at, 'epoch')").

All four index features unblocked by this set:
  - sort direction per column          (ColumnRefs[i].Direction)
  - expression indexes                 (Expression)
  - covering / INCLUDE columns         (Include)
  - per-column opclass                 (ColumnRefs[i].OpClass)
  - partial-index predicate            (Where, already present)

Designed for backwards compat: existing CRs using `Columns []string`
keep working unchanged. The Webhook validator (Phase 4 — separate
concern) enforces "exactly one of Columns/ColumnRefs/Expression".



_Appears in:_
- [DesiredMaterializedView](#desiredmaterializedview)
- [DesiredTable](#desiredtable)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the index. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `columns` _string array_ | Columns the index covers, in order. Simple form: one btree column<br />per entry, ASC sort, no opclass. Mutually exclusive with<br />ColumnRefs and Expression. |  | MaxItems: 32 <br />MinItems: 1 <br /> |
| `columnRefs` _[DesiredIndexColumn](#desiredindexcolumn) array_ | ColumnRefs is the rich-form column list. Each entry can specify<br />per-column sort direction (ASC/DESC), null ordering (NULLS FIRST/<br />LAST), and operator class (e.g. varchar_pattern_ops for LIKE<br />optimisation). Mutually exclusive with Columns and Expression. |  | MaxItems: 32 <br />MinItems: 1 <br /> |
| `expression` _string_ | Expression is a single index-expression body, used for expression<br />indexes like CREATE INDEX foo ON t (lower(email)). The string is<br />emitted verbatim wrapped in parens; do NOT include `CREATE INDEX`<br />or column-list parens. Mutually exclusive with Columns and<br />ColumnRefs. |  | MaxLength: 1024 <br /> |
| `include` _string array_ | Include is the optional INCLUDE clause for covering indexes<br />(PostgreSQL 11+). Listed columns are stored in the index leaf<br />pages but not used as the index key, enabling index-only scans<br />for queries that select these columns alongside an index probe.<br />Method MUST be btree (PG limitation). |  | MaxItems: 32 <br /> |
| `unique` _boolean_ | Unique controls UNIQUE constraint on the index. |  |  |
| `nullsNotDistinct` _boolean_ | NullsNotDistinct controls how NULL values are deduplicated in a<br />UNIQUE index (PostgreSQL 15+). Default behaviour (NULLS DISTINCT)<br />treats every NULL as distinct, allowing many rows with NULL in<br />the indexed column. NULLS NOT DISTINCT treats all NULLs as equal<br />for uniqueness — at most one row may have NULL.<br />Only meaningful when Unique=true. Ignored otherwise. |  |  |
| `method` _string_ | Method selects the PG index method (btree default, gin, gist,<br />hash, brin, spgist). INCLUDE only valid with btree. | btree | Enum: [btree hash gin gist brin spgist] <br /> |
| `where` _string_ | Where is an optional partial-index predicate. The string is<br />emitted verbatim after the WHERE keyword; do NOT include the<br />keyword itself. |  | MaxLength: 1024 <br /> |


#### DesiredIndexColumn



DesiredIndexColumn is one column entry in DesiredIndex.ColumnRefs.

Exactly one of Name or Expression must be set. Name is for direct
column references (the common case); Expression is for per-column
SQL expressions like `lower(email)` or `coalesce(rule_value, '')`.
PostgreSQL allows mixing column refs and expressions in a single
multi-column index — this struct accommodates either shape.



_Appears in:_
- [DesiredIndex](#desiredindex)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the column. Quoted as identifier on render. Mutually<br />exclusive with Expression. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br /> |
| `expression` _string_ | Expression is a per-column SQL expression body (e.g.<br />`coalesce(col, '')` or `lower(email)`). The renderer wraps it<br />in parens. Mutually exclusive with Name. Type-cast literals<br />like `'x'::uuid` and function calls are permitted; the bodies<br />are emitted verbatim — operator MUST treat them as<br />trusted-input (in practice keystonectl-inspect generates them<br />from pg_get_indexdef which is the SOURCE OF TRUTH from the<br />live database). |  | MaxLength: 1024 <br /> |
| `direction` _string_ | Direction selects the per-column sort order. PG default is asc;<br />desc materially changes the index for ORDER BY queries.<br />No `+kubebuilder:default` marker — apiserver-side defaulting<br />materialises the field on every stored object even when the<br />source omits it, which then surfaces as spurious drift to<br />SSA-based diff (every Argo sync reports OutOfSync because git<br />has no `direction` field while live has `direction: asc`). The<br />differ already treats the empty string as "asc" semantically<br />(see internal/migration/declarative/differ.go renderIndexColumn),<br />so the marker added behaviour the differ doesn't need but the<br />admission path materialised verbosely. |  | Enum: [asc desc] <br /> |
| `nulls` _string_ | Nulls selects null ordering. PG default depends on Direction<br />(asc -> nulls last; desc -> nulls first). Override here if the<br />query plans need a non-default placement. |  | Enum: [first last] <br /> |
| `opClass` _string_ | OpClass is an optional per-column operator class. Common cases:<br />  varchar_pattern_ops    — LIKE 'prefix%' optimisation<br />  text_pattern_ops       — same for text columns<br />  gin_trgm_ops           — pg_trgm fuzzy match<br />  jsonb_path_ops         — JSON @> queries on jsonb cols<br />The string is emitted verbatim as a quoted identifier. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br /> |


#### DesiredMaterializedView



DesiredMaterializedView declares a PostgreSQL materialized view.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the materialized view name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `query` _string_ | Query is the SELECT statement backing the materialized view. |  | MaxLength: 65536 <br />Required: \{\} <br /> |
| `indexes` _[DesiredIndex](#desiredindex) array_ | Indexes on the materialized view (optional, for query performance). |  | MaxItems: 32 <br /> |
| `withData` _boolean_ | WithData controls whether the view is populated on creation.<br />Default true. Set false for large views where you want to<br />REFRESH MATERIALIZED VIEW CONCURRENTLY on a schedule. | true |  |


#### DesiredPolicy



DesiredPolicy declares a PostgreSQL row-level security policy.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the policy name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `table` _string_ | Table is the table this policy applies to. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `command` _string_ | Command is the SQL command the policy applies to. | ALL | Enum: [ALL SELECT INSERT UPDATE DELETE] <br /> |
| `permissive` _boolean_ | Permissive controls whether the policy is permissive or<br />restrictive. Default true (PERMISSIVE). | true |  |
| `roles` _string array_ | Roles is the list of roles the policy applies to.<br />Empty or ["public"] means all roles. |  | MaxItems: 16 <br /> |
| `using` _string_ | Using is the USING expression (row visibility filter).<br />Applied to SELECT, UPDATE, DELETE. |  | MaxLength: 4096 <br /> |
| `withCheck` _string_ | WithCheck is the WITH CHECK expression (row write filter).<br />Applied to INSERT, UPDATE. |  | MaxLength: 4096 <br /> |


#### DesiredSequence



DesiredSequence declares a PostgreSQL sequence.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the sequence name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `dataType` _string_ | DataType is the sequence's data type (smallint, integer, bigint). | bigint | Enum: [smallint integer bigint] <br /> |
| `incrementBy` _integer_ | IncrementBy is the step value. | 1 | Minimum: 1 <br /> |
| `minValue` _integer_ | MinValue is the minimum value. 0 means PG default. |  |  |
| `maxValue` _integer_ | MaxValue is the maximum value. 0 means PG default. |  |  |
| `startWith` _integer_ | StartWith is the starting value. 0 means PG default. |  |  |
| `ownedBy` _string_ | OwnedBy ties the sequence to a column (schema "table.column" format).<br />When the owning column is dropped, the sequence is too. |  | MaxLength: 127 <br /> |


#### DesiredTable



DesiredTable describes one table's desired shape.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the table. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `columns` _[DesiredColumn](#desiredcolumn) array_ | Columns is the ordered list of columns. |  | MaxItems: 128 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `primaryKey` _string array_ | PrimaryKey is the composite primary-key column list. Use this for<br />multi-column PKs; single-column PKs can also use<br />DesiredColumn.PrimaryKey as a shorthand. |  | MaxItems: 8 <br /> |
| `indexes` _[DesiredIndex](#desiredindex) array_ | Indexes declared on this table. |  | MaxItems: 32 <br /> |
| `foreignKeys` _[DesiredForeignKey](#desiredforeignkey) array_ | ForeignKeys declared on this table. |  | MaxItems: 32 <br /> |
| `enableRLS` _boolean_ | EnableRLS enables row-level security on this table. When true,<br />the differ emits ALTER TABLE ... ENABLE ROW LEVEL SECURITY.<br />Policies are declared in spec.policies[]. |  |  |
| `forceRLS` _boolean_ | ForceRLS controls whether row-level security also applies to the<br />table's OWNER (ALTER TABLE ... FORCE ROW LEVEL SECURITY). Ignored<br />unless EnableRLS is true.<br />Unset means true. That default is deliberate and is the safe one:<br />PostgreSQL exempts a table's owner from its own policies, so<br />ENABLE-without-FORCE leaves every policy in place while enforcing<br />none of them against an application that connects as the owning role<br />— which is the ordinary deployment shape. A field that defaulted to<br />false would mean a SchemaDefinition asking for row-level security<br />got, silently, a schema that did not enforce it.<br />It is a pointer so that "unset" and "explicitly false" are<br />distinguishable. Explicitly false is a real configuration — it fits a<br />schema whose owner is a migration role that must retain unrestricted<br />access — and it is honoured, but it has to be asked for. Before this<br />field existed the differ emitted FORCE unconditionally alongside<br />ENABLE, so leaving it unset preserves exactly that behaviour and no<br />existing SchemaDefinition changes meaning. |  |  |
| `privileges` _[DesiredTablePrivilege](#desiredtableprivilege) array_ | Privileges declares per-table GRANT statements that should hold<br />against this table. Every entry expands at reconcile time to:<br />  GRANT <Privileges> ON TABLE <Name> TO <ToRole>;<br />Used to heal already-orphaned tables (created before<br />DefaultPrivileges was declared on the parent SchemaDefinition)<br />and to grant non-default privileges to specific roles.<br />DefaultPrivileges in the parent SchemaDefinitionSpec covers the<br />"every new table" case; this field is the per-table override. |  | MaxItems: 32 <br /> |
| `checkConstraints` _[DesiredCheckConstraint](#desiredcheckconstraint) array_ | CheckConstraints declares table-level CHECK constraints that must<br />hold for the table. Each entry expands at reconcile time to:<br />  ALTER TABLE <Name> ADD CONSTRAINT <Name> CHECK (<Definition>);<br />Drop is gated by SchemaDefinitionSpec.AllowDestructive — without<br />it, removed entries from this list produce a warning and are not<br />emitted, identical to the column-drop policy. Definition is<br />passed verbatim into the SQL CHECK clause; the SDK does NOT<br />canonicalise expressions, so an exact textual match against the<br />existing pg_constraint definition is required for the differ to<br />recognise an unchanged constraint as already-applied. |  | MaxItems: 32 <br /> |
| `uniqueConstraints` _[DesiredUniqueConstraint](#desireduniqueconstraint) array_ | UniqueConstraints declares table-level UNIQUE constraints. Prefer<br />these over a unique entry in Indexes whenever a foreign key might<br />reference the columns: PostgreSQL will not accept a bare unique<br />index as an FK target. |  | MaxItems: 32 <br /> |
| `primaryKeyName` _string_ | PrimaryKeyName names the PRIMARY KEY constraint. Empty lets<br />PostgreSQL derive it, which yields `<table>_pkey`.<br />Set it to reproduce a schema whose primary key was named<br />explicitly — every ORM does this, EF Core emits `PK_<Table>` — so<br />that an adopted schema round-trips under its own name rather than<br />silently acquiring PostgreSQL's default. The name is not merely<br />cosmetic: `ON CONFLICT ON CONSTRAINT` and `ALTER TABLE DROP<br />CONSTRAINT` both address it.<br />Naming the constraint forces the table-level `CONSTRAINT <name><br />PRIMARY KEY (...)` spelling, since the inline column shorthand has<br />nowhere to carry a name. |  | MaxLength: 63 <br /> |


#### DesiredTablePrivilege



DesiredTablePrivilege expresses one per-table GRANT statement.

Semantics:

	GRANT <Privileges> ON TABLE <table> TO <ToRole>;

Used to heal already-orphaned tables that were created before
DefaultPrivileges was declared on the parent SchemaDefinition, and
to grant non-default privileges to specific roles. Idempotent in
PostgreSQL.



_Appears in:_
- [DesiredTable](#desiredtable)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `toRole` _string_ | ToRole is the PG role receiving the granted privileges. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `privileges` _string array_ | Privileges is the privilege set granted. Same validation as<br />DesiredDefaultPrivilege.Privileges. |  | MaxItems: 16 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `withGrantOption` _boolean_ | WithGrantOption mirrors the SQL `WITH GRANT OPTION` clause. |  |  |


#### DesiredTrigger



DesiredTrigger declares a PostgreSQL trigger binding.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the trigger name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `table` _string_ | Table is the table the trigger is attached to. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `timing` _string_ | Timing is BEFORE or AFTER. |  | Enum: [BEFORE AFTER INSTEAD OF] <br />Required: \{\} <br /> |
| `events` _string array_ | Events is the list of events that fire the trigger. |  | MaxItems: 4 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `forEachRow` _boolean_ | ForEachRow controls whether the trigger fires per row or per<br />statement. Default true (FOR EACH ROW). | true |  |
| `function` _string_ | Function is the trigger function name (must exist in spec.functions<br />or already in the database). The function must return TRIGGER. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `when` _string_ | When is an optional conditional expression. The trigger only<br />fires when this evaluates to true. |  | MaxLength: 4096 <br /> |


#### DesiredUniqueConstraint



DesiredUniqueConstraint declares a table-level UNIQUE constraint.

Distinct from a unique DesiredIndex on purpose. PostgreSQL implements
a UNIQUE constraint with a unique index, so the two enforce the same
rule — but only a constraint can be the target of a foreign key
(`REFERENCES t (col)` requires a PRIMARY KEY or UNIQUE constraint, not
a bare unique index). Modelling a constraint as an index therefore
round-trips a schema into one where existing foreign keys can no
longer be created.



_Appears in:_
- [DesiredTable](#desiredtable)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the constraint. PostgreSQL names the backing index after<br />it, so the name must not collide with an index name either. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `columns` _string array_ | Columns is the ordered column list the constraint spans. Order is<br />significant: it determines the backing index's column order, and<br />therefore which prefix lookups the index can serve. |  | MaxItems: 32 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `nullsNotDistinct` _boolean_ | NullsNotDistinct selects `UNIQUE NULLS NOT DISTINCT`, where two<br />NULLs collide instead of being treated as different values<br />(PostgreSQL 15+).<br />This is a semantic difference, not a tuning knob: under the<br />default NULLS DISTINCT a nullable unique column accepts unlimited<br />NULL rows. Dropping the flag on round-trip would silently widen<br />what the table accepts. |  |  |


#### DesiredView



DesiredView declares a PostgreSQL view.



_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the view name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `query` _string_ | Query is the SELECT statement backing the view. |  | MaxLength: 65536 <br />Required: \{\} <br /> |
| `replace` _boolean_ | Replace controls CREATE OR REPLACE semantics. When true, the<br />differ always emits CREATE OR REPLACE VIEW regardless of whether<br />the view exists. Default false: only CREATE when absent,<br />otherwise warn if the query changed. |  |  |


#### DriftFinding



DriftFinding describes one delta between the recorded baseline and the
observed live schema. The DriftController fills the slice when it
detects drift; an empty slice means "drift detected by hash but
no per-object diff was computed yet" (Phase 5.1 will populate it).



_Appears in:_
- [DriftReportStatus](#driftreportstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _string_ | Kind classifies the finding.<br />The RLS* and Policy* kinds describe a change in ENFORCEMENT rather<br />than in shape. They are graded Critical when enforcement is lost<br />(see drift.Severity) because, unlike a dropped column, nothing about<br />the schema's shape looks wrong afterwards.<br />This enum gates what the controller may write: an unlisted kind fails<br />apiserver validation and takes the entire DriftReport patch with it,<br />so it must stay in step with the kinds drift.Diff emits. |  | Enum: [TableAdded TableDropped ColumnAdded ColumnDropped ColumnChanged IndexAdded IndexDropped ConstraintAdded ConstraintDropped RLSEnabled RLSDisabled RLSForced RLSUnforced PolicyAdded PolicyDropped PolicyChanged Other] <br />Required: \{\} <br /> |
| `object` _string_ | Object is the qualified PG object name (e.g. "crm.leads.score"). |  | MaxLength: 512 <br />Required: \{\} <br /> |
| `description` _string_ | Description is human-readable detail. |  | MaxLength: 2048 <br /> |


#### DriftReport



DriftReport records a divergence between a DatabaseSchema's recorded
baseline and the live PostgreSQL state. Created by the DriftController;
users acknowledge or resolve by deleting / labelling.



_Appears in:_
- [DriftReportList](#driftreportlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `DriftReport` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[DriftReportSpec](#driftreportspec)_ |  |  |  |
| `status` _[DriftReportStatus](#driftreportstatus)_ |  |  |  |


#### DriftReportList



DriftReportList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `DriftReportList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[DriftReport](#driftreport) array_ |  |  |  |


#### DriftReportSpec



DriftReportSpec is generated by the DriftController; users do NOT
author DriftReports directly.



_Appears in:_
- [DriftReport](#driftreport)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `databaseSchemaRef` _string_ | DatabaseSchemaRef is the namespaced name of the DatabaseSchema<br />the report is about. Same namespace as the report. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `logicalDatabaseRef` _string_ | LogicalDatabaseRef is duplicated from the schema so a single<br />`kubectl get driftreports -o wide` is useful without joining<br />across CR types. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `providerRef` _string_ | ProviderRef is duplicated from the LogicalDatabase for the same<br />reason. |  | MaxLength: 253 <br />Required: \{\} <br /> |


#### DriftReportStatus



DriftReportStatus reports the live observation.



_Appears in:_
- [DriftReport](#driftreport)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `severity` _[DriftSeverity](#driftseverity)_ | Severity grades the finding for routing. |  | Enum: [info warning critical] <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions:<br />  Detected   — controller observed a hash mismatch<br />  Acknowledged — operator marked the drift as accepted (Phase 5.1)<br />  Resolved   — drift was reconciled (live matches baseline again) |  |  |
| `detectedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | DetectedAt is when the controller first observed the drift. |  |  |
| `lastObservedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastObservedAt is the most recent inspection that confirmed the<br />drift still exists. |  |  |
| `expectedHash` _string_ | ExpectedHash is the baseline hash recorded after the last<br />successful migration (or operator acceptance). |  | MaxLength: 64 <br /> |
| `observedHash` _string_ | ObservedHash is the hash of the live schema at LastObservedAt. |  | MaxLength: 64 <br /> |
| `findings` _[DriftFinding](#driftfinding) array_ | Findings holds per-object deltas. Empty in Phase 5 (hash-only<br />detection); populated in Phase 5.1 when the inspector grows a<br />structural diff implementation. |  | MaxItems: 128 <br /> |


#### DriftSeverity

_Underlying type:_ _string_

DriftSeverity grades a DriftReport for triage routing.

_Validation:_
- Enum: [info warning critical]

_Appears in:_
- [DriftReportStatus](#driftreportstatus)

| Field | Description |
| --- | --- |
| `info` | DriftSeverityInfo — schema fingerprint changed but no objects<br />were dropped. Typical of operator-applied migrations the<br />MigrationController is about to record.<br /> |
| `warning` | DriftSeverityWarning — objects added, modified, or columns<br />re-typed without a corresponding MigrationExecution. Most<br />real drift sits here.<br /> |
| `critical` | DriftSeverityCritical — objects DROPPED. Highest priority;<br />pages oncall.<br /> |


#### DriftedSchema



DriftedSchema records one fanned-out DatabaseSchema that diverges
from the lockstep set under MixedVersionPolicy=Refuse.



_Appears in:_
- [SchemaDefinitionStatus](#schemadefinitionstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schemaRef` _string_ | SchemaRef is the DatabaseSchema's metadata.name. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `pendingOperations` _integer_ | PendingOperations is the diff size for this individual schema<br />against the SchemaDefinition's desired state. The schema with<br />the largest PendingOperations is the most-behind candidate. |  | Required: \{\} <br /> |
| `lastInspectedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastInspectedAt is when the reconciler last inspected this<br />schema's live shape. |  |  |


#### DropColumnOp



DropColumnOp drops a column. Two-phase: Expand records the deprecation
(status only); Contract issues DROP COLUMN.



_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the column to drop. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |


#### DropNotNullOp



DropNotNullOp is the payload for Kind=drop_not_null.



_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `column` _string_ | Column is the name of the column to transition to NULLABLE. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |


#### ExecutionMode

_Underlying type:_ _string_

ExecutionMode controls what a MigrationExecution does with the
bundle's SQL.

_Validation:_
- Enum: [Apply RecordOnly]

_Appears in:_
- [MigrationBundleSpec](#migrationbundlespec)
- [MigrationExecutionSpec](#migrationexecutionspec)

| Field | Description |
| --- | --- |
| `Apply` | ExecutionModeApply executes the bundle's SQL and records the<br />version in the schema's tracking table. Default.<br /> |
| `RecordOnly` | ExecutionModeRecordOnly upserts the (version, contentHash) row<br />into the tracking table WITHOUT executing any SQL. This is the<br />adoption-baseline primitive (ADR 0008 / ADR 0027): the schema's<br />current live state is declared as already-applied so subsequent<br />incremental migrations build on top of it. Identical semantics<br />to `flyway baseline`, Liquibase `changelog-sync`, and<br />`atlas migrate apply --baseline`.<br />Because a RecordOnly bundle never executes, execution-safety<br />lint findings are advisory: they are still published to<br />status.lintFindings but never block admission or reconcile.<br />Integrity (keystone.sum), per-version immutability, and<br />approval gates apply in full.<br /> |


#### ExecutionPhase

_Underlying type:_ _string_

ExecutionPhase is the coarse lifecycle for a MigrationExecution.

_Validation:_
- Enum: [Pending Running Expanding Expanded Contracting Aborting RollingBack Succeeded Failed Aborted RolledBack RollbackFailed]

_Appears in:_
- [MigrationExecutionStatus](#migrationexecutionstatus)

| Field | Description |
| --- | --- |
| `Pending` |  |
| `Running` |  |
| `Expanding` |  |
| `Expanded` |  |
| `Contracting` |  |
| `Aborting` | ExecutionPhaseAborting is transient — the controller is running<br />per-op abort handlers to roll back an in-flight Expand. On success<br />the phase advances to Aborted; on failure it sticks at Aborting<br />and the reconcile retries.<br /> |
| `Succeeded` |  |
| `Failed` |  |
| `Aborted` |  |
| `RollingBack` | ExecutionPhaseRollingBack is entered when autoRollback=true and<br />a down source exists. The controller resolves the down source<br />and applies it to undo the forward migration.<br /> |
| `RolledBack` | ExecutionPhaseRolledBack is the terminal state after a successful<br />rollback. The down source was applied and the schema_migrations<br />entry was removed.<br /> |
| `RollbackFailed` | ExecutionPhaseRollbackFailed is the terminal state when the<br />rollback itself fails. Manual intervention required.<br /> |


#### FileIntegrity



FileIntegrity is one entry surfaced from a verified keystone.sum.
Deliberately minimal — the sum file in Git is the authoritative
artifact; this slice exists for kubectl describe / dashboards.



_Appears in:_
- [IntegrityStatus](#integritystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the filename as it appears in keystone.sum. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `hash` _string_ | Hash is the hex-encoded SHA-256 recorded for this file. |  | MaxLength: 64 <br />Required: \{\} <br /> |


#### InstanceDatabase



InstanceDatabase reports the concrete LogicalDatabase + DatabaseSchema
resources the controller created for this instance. One entry per
ProductDatabase declared in the ProductDefinition.



_Appears in:_
- [ProductInstanceStatus](#productinstancestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ | Key matches ProductDatabase.Key. |  |  |
| `logicalDatabaseRef` _string_ | LogicalDatabaseRef is the namespaced name of the LogicalDatabase CR. |  |  |
| `databaseSchemaRefs` _string array_ | DatabaseSchemaRefs lists the DatabaseSchema CRs for this database. |  |  |


#### IntegrityPolicy



IntegrityPolicy governs how strictly the admission webhook reacts to
a MigrationBundle's keystone.sum state. Keep the surface small: the
webhook has two dimensions of discretion — "require the file at all"
and "reject on mismatch" — and both collapse to straightforward
booleans. Richer CEL-based policy lands in Phase C2.



_Appears in:_
- [SchemaPolicySpec](#schemapolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `requireSumFile` _boolean_ | RequireSumFile rejects any matched MigrationBundle whose resolved<br />source does not contain a keystone.sum. Default false — Phase A1<br />ships in advisory mode so existing bundles keep admitting while<br />authors add sums. Flip to true in Phase A3 once every bundle in<br />the cluster carries a sum. | false |  |


#### IntegrityStatus



IntegrityStatus is the observable outcome of the keystone.sum
verification performed at resolve time. The matching condition
ConditionTypeIntegrityVerified carries the True/False/Unknown signal;
this struct carries the detail tooling needs to triage.



_Appears in:_
- [MigrationBundleStatus](#migrationbundlestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `status` _string_ | Status is the outcome classification. One of:<br />  Valid      — keystone.sum present and every hash matches<br />  Missing    — no keystone.sum in the resolved source<br />  Malformed  — keystone.sum present but failed to parse<br />  Mismatch   — keystone.sum present but hashes diverge |  | Enum: [Valid Missing Malformed Mismatch] <br />Required: \{\} <br /> |
| `rootHash` _string_ | RootHash is the directory-level SHA-256 declared by keystone.sum.<br />Empty when Status=Missing. Surfaced so operators can grep audit<br />logs for a specific bundle snapshot. |  | MaxLength: 64 <br /> |
| `message` _string_ | Message is a human-readable detail. For Mismatch this names the<br />first divergent file; for Malformed it quotes the parse error.<br />Empty for Valid and Missing. |  | MaxLength: 512 <br /> |
| `files` _[FileIntegrity](#fileintegrity) array_ | Files carries per-file integrity entries as reported by<br />keystone.sum. Present only when Status=Valid — we do not surface<br />partial sums for Malformed/Mismatch because clients may act on<br />stale data. Empty on Missing. |  | MaxItems: 1024 <br /> |


#### IsolationMode

_Underlying type:_ _string_

IsolationMode mirrors the legacy db-provisioner's pool/bridge/silo
taxonomy. Each tenant of a product gets storage shaped according to
the chosen mode.

_Validation:_
- Enum: [pool bridge silo]

_Appears in:_
- [ProductDatabase](#productdatabase)
- [ProductDefinitionSpec](#productdefinitionspec)
- [ProductInstanceSpec](#productinstancespec)

| Field | Description |
| --- | --- |
| `pool` | IsolationModePool — shared database AND shared schema; per-tenant<br />data segregation enforced by row-level security policies on a<br />tenant_id column. Cheapest; only suitable for products with<br />strict data-shape uniformity and weak compliance ceiling.<br /> |
| `bridge` | IsolationModeBridge — shared database, schema-per-tenant. The<br />industry sweet spot: tenants are SQL-namespace isolated but share<br />connection pools, extensions, and admin overhead.<br /> |
| `silo` | IsolationModeSilo — database-per-tenant. Strongest isolation,<br />highest overhead; required for regulated workloads (HIPAA, PCI<br />large merchant) or air-gapped deployments.<br /> |


#### LifecycleHook



LifecycleHook describes a webhook the controller calls during
transitions. Phase 4 implements this as a stub (records intent in
status); Phase 4.1 wires real HTTP delivery with HMAC signing.



_Appears in:_
- [ProductDefinitionSpec](#productdefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `on` _string_ | On selects the transition that fires the hook. |  | Enum: [Activated Deactivated Failed] <br />Required: \{\} <br /> |
| `url` _string_ | URL is the receiver endpoint. HTTPS only — admission rejects http://. |  | MaxLength: 2048 <br />Pattern: `^https://` <br />Required: \{\} <br /> |
| `signingSecretRef` _string_ | SigningSecretRef references a Secret in keystone-system holding<br />the HMAC key used to sign the payload. |  |  |


#### LintFinding



LintFinding is a single squawk/conftest finding produced during the
plan phase.



_Appears in:_
- [MigrationBundleStatus](#migrationbundlestatus)
- [MigrationPlanSpec](#migrationplanspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `severity` _[LintLevel](#lintlevel)_ | Severity matches LintLevel: error \| warning \| notice. |  | Enum: [error warning notice] <br />Required: \{\} <br /> |
| `rule` _string_ | Rule is the linter's rule identifier (e.g. squawk's<br />"adding-required-field" or conftest's "deny-truncate"). |  | MaxLength: 128 <br />Required: \{\} <br /> |
| `file` _string_ | File is the SQL filename within the source where the finding fired. |  | MaxLength: 253 <br /> |
| `line` _integer_ | Line is the 1-indexed line in the source file. 0 if not applicable. |  | Minimum: 0 <br /> |
| `message` _string_ | Message is a human-readable description. |  | MaxLength: 4096 <br />Required: \{\} <br /> |


#### LintLevel

_Underlying type:_ _string_

LintLevel is the severity threshold above which lint findings cause the
admission webhook to reject a MigrationBundle.

_Validation:_
- Enum: [error warning notice]

_Appears in:_
- [ApprovalCondition](#approvalcondition)
- [LintFinding](#lintfinding)
- [SchemaPolicySpec](#schemapolicyspec)

| Field | Description |
| --- | --- |
| `error` |  |
| `warning` |  |
| `notice` |  |


#### LogicalDatabase



LogicalDatabase declares a PostgreSQL database that should exist on a
referenced cluster. The controller is idempotent: declaring the same
database twice is harmless; deleting the resource (with finalizer) will
drop the database only if spec.deletionPolicy permits it (Phase 4+).



_Appears in:_
- [LogicalDatabaseList](#logicaldatabaselist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `LogicalDatabase` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[LogicalDatabaseSpec](#logicaldatabasespec)_ |  |  |  |
| `status` _[LogicalDatabaseStatus](#logicaldatabasestatus)_ |  |  |  |


#### LogicalDatabaseList



LogicalDatabaseList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `LogicalDatabaseList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[LogicalDatabase](#logicaldatabase) array_ |  |  |  |


#### LogicalDatabaseSpec



LogicalDatabaseSpec describes one logical PostgreSQL database (a `CREATE
DATABASE` target). Multiple LogicalDatabases may live in the same
underlying cluster; the controller is responsible for opening admin
connections to the cluster, ensuring the DB exists with the requested
configuration, and creating the owner role.

LogicalDatabases are namespaced because a tenant or product team may own
the DB and operate on it through Keystone's RBAC. Cross-namespace access
is allowed only through SchemaPolicy.



_Appears in:_
- [LogicalDatabase](#logicaldatabase)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is the actual PostgreSQL database name. Distinct from<br />metadata.name: PG identifiers commonly use underscores (e.g.<br />example_suite) while Kubernetes resources prefer hyphens<br />(hexxlock-erp). Required so this never has to be inferred. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `clusterRef` _string_ | ClusterRef is the name of the ClusterRegistration this database<br />belongs to from a workload-rollout perspective. Used by the<br />RolloutController (Phase 7) and DriftController (Phase 5) to scope<br />per-cluster operations. NOT used by the SchemaController for<br />connectivity — that goes via ProviderRef. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `providerRef` _string_ | ProviderRef is the name of the DatabaseProvider hosting this<br />database. The SchemaController opens admin sessions against this<br />provider to CREATE DATABASE / CREATE ROLE / CREATE EXTENSION. A<br />LogicalDatabase belongs to exactly one provider; cross-provider<br />replication is out of scope (use a ReplicationPolicy in Phase 6+). |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `deletionPolicy` _string_ | DeletionPolicy controls what happens to the underlying PostgreSQL<br />database when the LogicalDatabase resource is deleted. Defaults to<br />Retain — deleting a CR never drops production data. Operators must<br />explicitly opt in to Delete for tear-down workflows. | Retain | Enum: [Retain Delete] <br /> |
| `ownerRole` _string_ | OwnerRole is the PostgreSQL role that owns the database. The<br />controller creates the role if absent and grants it CREATE on the<br />DB. Required because creating a database with no clear owner is an<br />operational landmine. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `extensions` _string array_ | Extensions are PostgreSQL extensions to CREATE EXTENSION IF NOT<br />EXISTS. Order is preserved. The admission webhook rejects any<br />extension not present in the cluster's allow-list (declared on the<br />ClusterRegistration's spec.labels). |  | MaxItems: 32 <br /> |
| `encoding` _string_ | Encoding overrides the default character encoding (UTF8). Setting<br />this on an existing database is a no-op — encoding is fixed at<br />CREATE time. | UTF8 | Enum: [UTF8 SQL_ASCII LATIN1] <br /> |
| `collation` _string_ | Collation overrides the default LC_COLLATE. Like Encoding, this is<br />fixed at CREATE time. | C | MaxLength: 64 <br /> |
| `ctype` _string_ | Ctype overrides the default LC_CTYPE. Fixed at CREATE time. | C | MaxLength: 64 <br /> |
| `connectionLimit` _integer_ | ConnectionLimit caps the maximum concurrent connections (PostgreSQL<br />`CONNECTION LIMIT` clause). -1 means unlimited (PostgreSQL default). | -1 | Minimum: -1 <br /> |
| `trackingTableName` _string_ | TrackingTableName is the table name Keystone writes migration<br />bookkeeping into. Defaults to "schema_migrations" — the<br />golang-migrate convention — which is the right choice on greenfield<br />databases.<br />Override when adopting Keystone on a database that already has a<br />migrator managing its own `schema_migrations`. Falcon-ID is the<br />canonical example: its existing table has `version INTEGER PK`<br />while Keystone's runner uses `version TEXT PK`. Setting<br />trackingTableName to "keystone_schema_migrations" lets both<br />trackers co-exist without touching production history. | schema_migrations | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br /> |
| `ownerRolePasswordSecretRef` _[SecretKeySelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#secretkeyselector-v1-core)_ | OwnerRolePasswordSecretRef references a Kubernetes Secret containing<br />the password the reconciler should set on OwnerRole when creating it<br />(and when rotating it on subsequent reconciles where the Secret<br />content changes).<br />The Secret MUST exist in the SAME namespace as the LogicalDatabase<br />resource. The reconciler reads spec.ownerRolePasswordSecretRef.name<br />from that namespace and reads the value at spec.ownerRolePassword<br />SecretRef.key. Cross-namespace references are intentionally<br />disallowed — the LogicalDatabase namespace gates who can change the<br />password.<br />When unset, EnsureRole creates the role WITHOUT a password. The<br />PostgreSQL admin can then assign one out-of-band (e.g. via Vault<br />dynamic secrets engine, manual psql, or any other mechanism) — the<br />pre-passwordSecretRef behavior. Backward-compatible: existing<br />LogicalDatabases without this field reconcile identically to v0.1.53.<br /># Standalone-product design note<br />SecretKeySelector is a vanilla corev1 type — the same pattern used<br />by cert-manager (Issuer.spec.vault.auth.appRole.secretRef),<br />CloudNativePG (Cluster.spec.bootstrap.initdb.passwordSecret),<br />Crunchy Postgres Operator (PostgresCluster.spec.users[].password.<br />secretName), Zalando Postgres Operator (postgresql.spec.users) and<br />many others. No vendor lock-in: any Secret-source operator<br />(External Secrets, kubernetes-external-secrets, Vault Agent<br />Injector, Reflector, plain kubectl-created Secrets) can populate<br />the referenced Secret and Keystone reads it the same way.<br /># Rotation<br />On every reconcile the controller compares the password in the<br />referenced Secret with what was last written to the role<br />(cached in status.observedPasswordHash). On change, the<br />controller issues ALTER ROLE … PASSWORD '<new>' to rotate the<br />PostgreSQL password atomically. ExternalSecrets/Vault rotation<br />flows therefore Just Work: rotate the source → ESO refreshes the<br />Secret → next reconcile applies ALTER ROLE. |  |  |


#### LogicalDatabaseStatus



LogicalDatabaseStatus reports observed state.



_Appears in:_
- [LogicalDatabase](#logicaldatabase)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state. Ready, Available, Progressing<br />and Degraded follow Kubernetes conventions; the controller may add<br />more (e.g. ExtensionsInstalled). |  |  |
| `resolvedEndpoint` _string_ | ResolvedEndpoint is the host:port the controller resolved from the<br />referenced ClusterRegistration. Surfaced in status so operators can<br />confirm where the controller is actually connecting. |  |  |
| `sizeBytes` _[Quantity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#quantity-resource-api)_ | SizeBytes is the on-disk size of the database, refreshed periodically<br />by the SchemaController. Approximate (snapshot at last reconcile). |  |  |
| `lastReconcileTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastReconcileTime is when the controller last successfully reconciled<br />this resource. Used to detect stuck reconciliations. |  |  |
| `observedPasswordHash` _string_ | ObservedPasswordHash is the SHA-256 (hex) of the password the<br />controller most recently applied to spec.ownerRole via CREATE ROLE<br />or ALTER ROLE PASSWORD. Compared against the live password in the<br />referenced spec.ownerRolePasswordSecretRef on each reconcile so<br />rotation (Secret content change) triggers exactly one ALTER ROLE<br />PASSWORD per change.<br />Empty when spec.ownerRolePasswordSecretRef is unset, OR when the<br />role was created with no password (legacy / out-of-band-password<br />path).<br />Stored as a hash, not the password itself, so leaking status never<br />leaks credentials. |  | MaxLength: 64 <br /> |


#### MigrationBundle



MigrationBundle declares a versioned set of SQL migrations. The
MigrationController reads it, generates per-target MigrationPlans,
then per-target MigrationExecutions.



_Appears in:_
- [MigrationBundleList](#migrationbundlelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `MigrationBundle` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[MigrationBundleSpec](#migrationbundlespec)_ |  |  |  |
| `status` _[MigrationBundleStatus](#migrationbundlestatus)_ |  |  |  |


#### MigrationBundleList



MigrationBundleList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `MigrationBundleList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[MigrationBundle](#migrationbundle) array_ |  |  |  |


#### MigrationBundleSpec



MigrationBundleSpec describes a versioned set of SQL migrations that
the controller should apply to one or more target schemas.

The bundle is immutable in spirit: once a Version is applied, it is
recorded in the target's schema_migrations table and the same Version
must never carry different SQL. The admission webhook enforces this by
computing a content hash and rejecting changes that try to overwrite
an applied version.



_Appears in:_
- [MigrationBundle](#migrationbundle)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `version` _string_ | Version is the bundle's monotonically-increasing identifier.<br />Display-friendly (semver, date, or integer) but ordering is<br />lexicographic; pad if you mix lengths. Required.<br />MaxLength bumped 64 → 253 (2026-05-20) to accommodate the<br />content-addressed bundle naming introduced in commit 372de32:<br />SchemaDefinitionReconciler emits bundles with<br />`<sd.Name>-declarative-<64-hex-hash>` which exceeds 64 bytes<br />for any SD name longer than a few characters. 253 matches the<br />Kubernetes resource-name limit (RFC 1123 subdomain), which is<br />the natural upper bound on these identifiers anyway. |  | MaxLength: 253 <br />Pattern: `^[a-zA-Z0-9._-]+$` <br />Required: \{\} <br /> |
| `strategy` _[MigrationStrategy](#migrationstrategy)_ | Strategy controls how migrations are applied. Defaults to<br />versioned; pgroll-expand-contract is Phase 6. | versioned | Enum: [versioned declarative pgroll-expand-contract] <br /> |
| `executionMode` _[ExecutionMode](#executionmode)_ | ExecutionMode selects between executing the bundle's SQL (Apply,<br />default) and recording it as already-applied without execution<br />(RecordOnly — the adoption-baseline path; see the ExecutionMode<br />type docs). Immutable once set: switching a shipped bundle<br />between modes would rewrite the meaning of its tracking-table<br />row. Requires strategy=versioned and autoRollback=false<br />(enforced at admission). | Apply | Enum: [Apply RecordOnly] <br /> |
| `source` _[MigrationSource](#migrationsource)_ | Source declares where the SQL files come from. Required for<br />strategy=versioned. Mutually exclusive with Operations. |  |  |
| `downSource` _[MigrationSource](#migrationsource)_ | DownSource is the optional rollback counterpart to Source. When<br />provided, the controller resolves it the same way as Source<br />(ConfigMap or OCI) and stores the resulting SQL in<br />MigrationExecution.status.reversalStatements. If AutoRollback is<br />also true, a Failed execution automatically transitions to<br />RollingBack and applies the down source.<br />Convention: down files use *.down.sql pattern. |  |  |
| `autoRollback` _boolean_ | AutoRollback, when true and DownSource is present, causes a<br />Failed MigrationExecution to automatically transition to<br />RollingBack. The controller resolves the DownSource and applies<br />it to undo the forward migration. If the rollback itself fails,<br />the execution transitions to RollbackFailed (manual intervention<br />required).<br />Default false — operators must explicitly opt in to automatic<br />rollback. Forward-fix is still the recommended approach for most<br />migration failures. | false |  |
| `operations` _[MigrationOperation](#migrationoperation) array_ | Operations declares pgroll-style expand/contract operations.<br />Required for strategy=pgroll-expand-contract. Mutually exclusive<br />with Source. The MigrationController applies operations in array<br />order during Expand phase, then in REVERSE order during Contract<br />phase (so e.g. a constraint added in op[0] is validated before<br />the column it depends on is dropped in op[1]'s contract). |  | MaxItems: 16 <br /> |
| `schemaSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#labelselector-v1-meta)_ | SchemaSelector picks the DatabaseSchemas this bundle applies to.<br />Standard label selector matched against DatabaseSchema.metadata.labels.<br />A bundle with an empty selector applies to nothing — refused at<br />admission to prevent accidental no-ops. |  | Required: \{\} <br /> |
| `policyRef` _string_ | PolicyRef optionally pins the SchemaPolicy this bundle is<br />validated against. When unset, every SchemaPolicy whose<br />targetSelector matches applies (intersection-of-stricter rules). |  | MaxLength: 253 <br /> |
| `rolloutPolicyRef` _string_ | RolloutPolicyRef optionally pins the staged-rollout policy that<br />the MigrationController uses to fan out executions. When unset<br />the bundle fans out to all matched schemas at once (the Phase 3<br />behaviour, kept for backward compatibility). |  | MaxLength: 253 <br /> |
| `approval` _object (keys:string, values:string)_ | Approval is metadata the admission webhook validates against<br />SchemaPolicy.spec.requireApproval. Each key matches a<br />requireApproval entry; the value MUST be a base64-encoded<br />signature from a CI job that ran in a CODEOWNERS-approved MR. |  |  |
| `maxConcurrentExecutions` _integer_ | MaxConcurrentExecutions caps how many MigrationExecutions the<br />bundle fans out in parallel when rolloutPolicyRef is unset<br />(non-staged path). Staged bundles ignore this field and use<br />RolloutPolicy.stages[].parallelism instead.<br />Default 0 means unlimited — every matched schema gets an<br />Execution on the first reconcile tick, preserving pre-B2<br />behaviour. Non-zero values throttle the fanout so reconcile<br />creates at most N Executions per tick and schedules the rest<br />on subsequent ticks as in-flight Executions complete. | 0 | Maximum: 256 <br />Minimum: 0 <br /> |
| `parallelism` _integer_ | Parallelism controls intra-bundle operation parallelism for the<br />pgroll-expand-contract strategy. Ops are grouped into "waves"<br />by table independence — all ops in a wave target distinct<br />Tables and run concurrently; a new wave starts when the previous<br />wave commits. Default 0 means fully sequential (pre-B3<br />behaviour); 1 is identical to 0; 2+ enables wave concurrency up<br />to the declared cap.<br />Ignored for strategy=versioned — SQL files stay sequential<br />because Keystone does not parse SQL to analyse table<br />dependencies. See docs/adrs/0020 for the wave scheduler<br />rationale. | 0 | Maximum: 16 <br />Minimum: 0 <br /> |


#### MigrationBundleStatus



MigrationBundleStatus reports rollout progress across all selected
schemas.



_Appears in:_
- [MigrationBundle](#migrationbundle)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state.<br />  Ready   — all matched schemas applied or out-of-scope.<br />  Linted  — squawk/conftest passed; only set after the lint phase.<br />  Planned — at least one MigrationPlan has been created. |  |  |
| `matchedSchemas` _integer_ | MatchedSchemas is the count of DatabaseSchemas currently selected.<br />Recomputed on every reconcile. |  |  |
| `appliedSchemas` _integer_ | AppliedSchemas is the count of those schemas where the bundle's<br />version is recorded as applied. |  |  |
| `lastPlanTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastPlanTime is when the controller last generated a MigrationPlan<br />from this bundle. |  |  |
| `currentStage` _string_ | CurrentStage is the active rollout stage when a RolloutPolicy is<br />in effect. Empty when the bundle uses no policy. |  | MaxLength: 63 <br /> |
| `stageHistory` _[StageProgress](#stageprogress) array_ | StageHistory records progress through each stage of the<br />referenced RolloutPolicy. Append-only. |  | MaxItems: 16 <br /> |
| `lintFindings` _[LintFinding](#lintfinding) array_ | LintFindings holds analyzer output from the plan phase. Populated<br />on every reconcile by the default analyzer registry (20 rules<br />today; configurable via SchemaPolicy in a later phase).<br />Cleared when no findings. Findings at severity >= "error" block<br />Ready=True. |  | MaxItems: 512 <br /> |
| `contentHash` _string_ | ContentHash is the SHA-256 of the resolved source content at the<br />last reconcile. Used by the admission webhook to detect<br />version-pinning violations (same Version, different SQL). |  | MaxLength: 64 <br /> |
| `integrity` _[IntegrityStatus](#integritystatus)_ | Integrity reports the outcome of verifying the bundle's resolved<br />source against its committed keystone.sum sidecar. Populated on<br />every reconcile. Empty when the bundle has never been resolved. |  |  |
| `approval` _[ApprovalSummary](#approvalsummary)_ | Approval reports the outcome of evaluating the bundle against<br />every matched SchemaPolicy.spec.approvalPolicies. Populated on<br />every reconcile; empty when no matching policy declares approval<br />rules. The matching condition ConditionTypeApproved carries the<br />True/False/Unknown signal; this struct carries the per-policy<br />detail that operators need to triage "why isn't my bundle<br />admitting?" from kubectl describe alone. |  |  |
| `schemaProgress` _[SchemaProgress](#schemaprogress) array_ | SchemaProgress is the per-schema rollout breakdown — one entry<br />per DatabaseSchema matched by spec.schemaSelector. Populated on<br />every reconcile; stale entries (schema no longer matched) are<br />pruned. Used for "kubectl describe migrationbundle" visibility<br />and dashboards that need per-tenant granularity rather than the<br />aggregate matched/applied counts above.<br />Capped at 256 entries so the status CR stays within etcd's<br />1.5 MiB value-size ceiling even on large multi-tenant fanouts;<br />bundles matching more than 256 schemas must rely on the<br />per-execution MigrationExecution CR list for full visibility. |  | MaxItems: 256 <br /> |


#### MigrationConfigMapSource



MigrationConfigMapSource references a ConfigMap holding SQL files.



_Appears in:_
- [MigrationSource](#migrationsource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the ConfigMap. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `filePattern` _string_ | FilePattern filters which keys are treated as SQL files. Defaults<br />to `*.up.sql`. | *.up.sql | MaxLength: 64 <br /> |


#### MigrationExecution



MigrationExecution is the per-target run. Created by the
MigrationController from a MigrationPlan that passed admission.



_Appears in:_
- [MigrationExecutionList](#migrationexecutionlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `MigrationExecution` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[MigrationExecutionSpec](#migrationexecutionspec)_ |  |  |  |
| `status` _[MigrationExecutionStatus](#migrationexecutionstatus)_ |  |  |  |


#### MigrationExecutionList



MigrationExecutionList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `MigrationExecutionList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[MigrationExecution](#migrationexecution) array_ |  |  |  |


#### MigrationExecutionSpec



MigrationExecutionSpec is the immutable definition of one run.
Generated by the MigrationController; users do NOT author executions
directly.



_Appears in:_
- [MigrationExecution](#migrationexecution)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `planRef` _string_ | PlanRef is the MigrationPlan this execution is realising. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `bundleRef` _string_ | BundleRef + BundleVersion duplicate spec from the plan for<br />audit clarity (Plans get garbage-collected; Executions are kept<br />for the audit retention window). |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `bundleVersion` _string_ | MaxLength 253 mirrors MigrationBundle.spec.version + MigrationPlan<br />.spec.bundleVersion (kept symmetric to accommodate the content-<br />addressed naming `<sd.Name>-declarative-<contentHash>` introduced<br />in 372de32). 64 char limit on this field would silently reject<br />Executions for Plans/Bundles that the upstream CRDs accept. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `schemaRef` _string_ | SchemaRef is the DatabaseSchema being mutated. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `contentHash` _string_ | ContentHash is the SHA-256 of the SQL content the plan was<br />generated from. The runner re-resolves the source and refuses<br />to apply if the hash drifted (paranoia gate against<br />time-of-check / time-of-use bugs). |  | MaxLength: 64 <br />Required: \{\} <br /> |
| `executionMode` _[ExecutionMode](#executionmode)_ | ExecutionMode is frozen from the bundle at fan-out time, like<br />ContentHash, so a later bundle mutation cannot change what an<br />in-flight execution does. RecordOnly executions upsert the<br />tracking-table row without running any SQL (adoption baseline,<br />ADR 0027). Empty is equivalent to Apply (pre-ADR-0027 objects). | Apply | Enum: [Apply RecordOnly] <br /> |


#### MigrationExecutionStatus



MigrationExecutionStatus reports the run.



_Appears in:_
- [MigrationExecution](#migrationexecution)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `phase` _[ExecutionPhase](#executionphase)_ | Phase is the coarse lifecycle. |  | Enum: [Pending Running Expanding Expanded Contracting Aborting RollingBack Succeeded Failed Aborted RolledBack RollbackFailed] <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions:<br />  Started   — controller picked it up and acquired the schema lock.<br />  Applied   — every statement ran successfully and was committed.<br />  Succeeded — Applied + version recorded in schema_migrations table.<br />  Failed    — at least one statement errored; transaction rolled back. |  |  |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | StartTime is when the runner began the apply transaction. |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | CompletionTime is when the runner exited (success or failure). |  |  |
| `applied` _[AppliedStatement](#appliedstatement) array_ | Applied lists what ran. Empty for failed-before-first-statement. |  | MaxItems: 2048 <br /> |
| `failedAtFile` _string_ | FailedAtFile + FailedAtIndex pinpoint the first failing statement. |  |  |
| `failedAtIndex` _integer_ |  |  | Minimum: 0 <br /> |
| `failureMessage` _string_ | FailureMessage is the PostgreSQL error verbatim. Truncated to<br />4 KiB to keep the CR manageable. |  | MaxLength: 4096 <br /> |
| `reversalStatements` _string array_ | ReversalStatements is the list of SQL statements that would undo<br />the forward migration. Populated by the declarative differ<br />(Plan.ReverseStatements) or from a resolved down source. Index-<br />aligned with the forward statements when generated by the differ.<br />Empty string entries indicate non-reversible operations (data<br />loss from DROP COLUMN, etc.). Used for emergency manual rollback<br />or by the auto-rollback controller. |  | MaxItems: 2048 <br /> |


#### MigrationOCISource



MigrationOCISource references an OCI artifact containing SQL files.



_Appears in:_
- [MigrationSource](#migrationsource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `repository` _string_ | Repository is the full OCI reference without the tag/digest,<br />e.g., "ghcr.io/dogukanturhal/migrations/billing". |  | MaxLength: 512 <br />Required: \{\} <br /> |
| `tag` _string_ | Tag is the OCI tag to pull. Mutually exclusive with Digest.<br />When both are empty, defaults to "latest". |  | MaxLength: 128 <br /> |
| `digest` _string_ | Digest is the OCI manifest digest (e.g., sha256:abc123...).<br />Preferred over Tag for immutable references. When set, Tag is<br />ignored. |  | MaxLength: 128 <br />Pattern: `^(sha256:[0-9a-f]\{64\})?$` <br /> |
| `filePattern` _string_ | FilePattern filters which files from the artifact are treated<br />as SQL migrations. Defaults to "*.up.sql". | *.up.sql | MaxLength: 64 <br /> |
| `pullSecretRef` _string_ | PullSecretRef names the Secret containing OCI registry<br />credentials. The Secret must have .dockerconfigjson data<br />(type kubernetes.io/dockerconfigjson) or plain username/password<br />keys. Looked up in the MigrationBundle's namespace. |  | MaxLength: 253 <br /> |
| `plainHTTP` _boolean_ | PlainHTTP allows pulling from registries without TLS. Default<br />false. Only set to true for development registries. |  |  |


#### MigrationOperation



MigrationOperation is one declarative pgroll-style operation. The
MigrationBundle authoring contract is "exactly one operation per
MigrationBundle" so plans + status + Argo CD diffs stay readable.
Multi-op bundles are deferred to Phase 6.1.



_Appears in:_
- [MigrationBundleSpec](#migrationbundlespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _[MigrationOperationKind](#migrationoperationkind)_ | Kind selects which sub-spec the controller reads. |  | Enum: [add_column drop_column rename_column add_constraint alter_column_type set_not_null drop_not_null] <br />Required: \{\} <br /> |
| `table` _string_ | Table is the target table the operation acts on. Required for<br />every kind today. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `addColumn` _[AddColumnOp](#addcolumnop)_ | AddColumn is populated when Kind=add_column. |  |  |
| `dropColumn` _[DropColumnOp](#dropcolumnop)_ | DropColumn is populated when Kind=drop_column. |  |  |
| `renameColumn` _[RenameColumnOp](#renamecolumnop)_ | RenameColumn is populated when Kind=rename_column. |  |  |
| `addConstraint` _[AddConstraintOp](#addconstraintop)_ | AddConstraint is populated when Kind=add_constraint. |  |  |
| `alterColumnType` _[AlterColumnTypeOp](#altercolumntypeop)_ | AlterColumnType is populated when Kind=alter_column_type. |  |  |
| `setNotNull` _[SetNotNullOp](#setnotnullop)_ | SetNotNull is populated when Kind=set_not_null. |  |  |
| `dropNotNull` _[DropNotNullOp](#dropnotnullop)_ | DropNotNull is populated when Kind=drop_not_null. |  |  |


#### MigrationOperationKind

_Underlying type:_ _string_

MigrationOperationKind is the discriminator for the per-operation
payload. Each kind has its own spec sub-struct; only the matching one
must be populated. Admission rejects multi-spec operations.

_Validation:_
- Enum: [add_column drop_column rename_column add_constraint alter_column_type set_not_null drop_not_null]

_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description |
| --- | --- |
| `add_column` | MigrationOperationAddColumn adds a column to an existing table.<br />In Expand phase: ALTER TABLE ADD COLUMN (nullable). Apps that<br />don't reference the new column are unaffected. In Contract phase:<br />optionally SET NOT NULL after the operator confirms backfill.<br /> |
| `drop_column` | MigrationOperationDropColumn drops a column. In Expand phase: no<br />schema change — the column is marked deprecated in the bundle's<br />metadata, giving apps time to deploy without referencing it. In<br />Contract phase: ALTER TABLE DROP COLUMN.<br /> |
| `rename_column` | MigrationOperationRenameColumn renames a column. In Expand phase:<br />ADD new column, COPY existing values, INSTALL trigger that mirrors<br />writes between the two columns so apps reading either name see<br />fresh data. In Contract phase: DROP TRIGGER, DROP old column.<br /> |
| `add_constraint` | MigrationOperationAddConstraint adds a CHECK or FOREIGN KEY<br />constraint without locking the table. In Expand phase: ALTER<br />TABLE ADD CONSTRAINT … NOT VALID (no full-table scan, no AccessExclusive<br />lock); in Contract phase: ALTER TABLE … VALIDATE CONSTRAINT<br />(needs SHARE UPDATE EXCLUSIVE — concurrent reads + writes<br />permitted; the validation scan can take a long time on big<br />tables but does not block traffic).<br /> |
| `alter_column_type` | MigrationOperationAlterColumnType changes a column's type via the<br />shadow-column pattern. Expand: ADD shadow column with new type;<br />COPY converted data; INSTALL trigger to mirror writes both ways.<br />Contract: DROP old column + RENAME shadow → original. Behaves<br />identically to rename_column under the hood, but the new column<br />is created with a different type.<br /> |
| `set_not_null` | MigrationOperationSetNotNull makes an existing nullable column<br />NOT NULL without a full-table AccessExclusive lock.<br />Expand: ADD CONSTRAINT chk_<col>_notnull CHECK (<col> IS NOT NULL)<br />NOT VALID — flags future writes but scans no rows. Contract:<br />VALIDATE CONSTRAINT (SHARE UPDATE EXCLUSIVE), then SET NOT NULL<br />(instant because PG recognises the proven CHECK), then DROP<br />CONSTRAINT (redundant once NOT NULL is in place).<br /> |
| `drop_not_null` | MigrationOperationDropNotNull removes a NOT NULL constraint.<br />Single-phase: Expand issues ALTER COLUMN DROP NOT NULL (a metadata-<br />only operation under AccessExclusive lock that PG holds only long<br />enough to flip the flag). Contract is a no-op. Abort is<br />best-effort: once DROP NOT NULL ran, NULL rows may have been<br />inserted, so re-establishing NOT NULL is not a clean rollback —<br />the abort handler records a warning and does not re-apply the<br />constraint.<br /> |


#### MigrationPlan



MigrationPlan is a controller-generated dry-run snapshot for one
(MigrationBundle, DatabaseSchema) pair. Users do not create these
directly; reviewers use them to inspect what is about to happen.



_Appears in:_
- [MigrationPlanList](#migrationplanlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `MigrationPlan` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[MigrationPlanSpec](#migrationplanspec)_ |  |  |  |
| `status` _[MigrationPlanStatus](#migrationplanstatus)_ |  |  |  |


#### MigrationPlanList



MigrationPlanList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `MigrationPlanList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[MigrationPlan](#migrationplan) array_ |  |  |  |


#### MigrationPlanSpec



MigrationPlanSpec is generated by the MigrationController; users do
NOT author plans directly. Spec is intentionally narrow because a plan
is a snapshot of a (bundle, schema) pair at a point in time.



_Appears in:_
- [MigrationPlan](#migrationplan)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `bundleRef` _string_ | BundleRef is the MigrationBundle this plan was generated from. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `bundleVersion` _string_ | BundleVersion freezes the bundle version at the moment the plan<br />was generated. If the bundle changes after the plan is created,<br />the plan stays accurate to its snapshot — a new plan is generated<br />for the new version.<br />MaxLength 253 mirrors MigrationBundle.spec.version (bumped in<br />keystone!109) — bundleVersion must accommodate the content-<br />addressed naming `<sd.Name>-declarative-<contentHash>` introduced<br />in 372de32. Asymmetric limits between Bundle.spec.version and<br />Plan.spec.bundleVersion would silently reject Plans for SDs<br />whose names + hash combo exceed 64 chars even though the upstream<br />Bundle was accepted. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `schemaRef` _string_ | SchemaRef is the DatabaseSchema this plan targets. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `statements` _[PlannedStatement](#plannedstatement) array_ | Statements is the ordered list of DDL the controller intends to<br />run. Truncated per-statement to 4 KiB; for review of full SQL,<br />inspect the source ConfigMap referenced by the bundle. |  | MaxItems: 2048 <br /> |
| `findings` _[LintFinding](#lintfinding) array_ | Findings holds lint output. The MigrationController refuses to<br />graduate the plan into a MigrationExecution if any finding has<br />severity >= the matched SchemaPolicy's MinLintLevel. |  | MaxItems: 512 <br /> |
| `approved` _boolean_ | Approved is the explicit Plan-Gate-Apply gate. The<br />MigrationPlanReconciler sets this to true automatically when the<br />matched SchemaPolicies resolve to approvalMode=Auto (the default<br />for backward compatibility); in Manual mode, a human or CI<br />principal flips it after reviewing spec.statements. The<br />MigrationExecution controller refuses to apply SQL until this<br />field is true.<br />Default false so new plans stop at the gate unless a policy<br />explicitly opts into auto-approval. Existing deployments<br />without a SchemaPolicy declaring planApproval fall back to<br />Auto — matches pre-A4 behavior. | false |  |


#### MigrationPlanStatus



MigrationPlanStatus reports whether the plan was approved and what
happened next.



_Appears in:_
- [MigrationPlan](#migrationplan)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions:<br />  Linted     — lint phase complete; findings populated.<br />  Approved   — plan passed all matched SchemaPolicies + admission gate.<br />  Executing  — at least one MigrationExecution has been created.<br />  Succeeded  — every spawned MigrationExecution is Succeeded. |  |  |
| `executionRef` _string_ | ExecutionRef is the MigrationExecution that this plan spawned.<br />Empty until the plan is approved and an execution is created. |  | MaxLength: 253 <br /> |
| `approvalMode` _[PlanApprovalMode](#planapprovalmode)_ | ApprovalMode is the effective mode the MigrationPlanReconciler<br />resolved from matched SchemaPolicies. Populated on every<br />reconcile. Observability-only — the authoritative gate is<br />spec.approved. |  | Enum: [Auto Manual] <br /> |
| `approvalPolicyName` _string_ | ApprovalPolicyName names the SchemaPolicy that selected the<br />current approvalMode. Empty when no matched policy declared<br />planApproval (i.e. the default Auto was taken). Useful for<br />answering "why is this plan waiting?" from kubectl describe. |  | MaxLength: 253 <br /> |


#### MigrationSource



MigrationSource describes where the controller should read SQL files.
Exactly one of ConfigMapRef / OCIArtifactRef / GitRef must be set,
matching Type.



_Appears in:_
- [MigrationBundleSpec](#migrationbundlespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[MigrationSourceType](#migrationsourcetype)_ | Type discriminates the source layer. |  | Enum: [ConfigMap OCIArtifact Git] <br />Required: \{\} <br /> |
| `configMapRef` _[MigrationConfigMapSource](#migrationconfigmapsource)_ | ConfigMapRef is required when Type=ConfigMap. Lookup is in the<br />same namespace as the MigrationBundle. |  |  |
| `ociArtifactRef` _[MigrationOCISource](#migrationocisource)_ | OCIArtifactRef is required when Type=OCIArtifact. Points to an<br />OCI artifact in a registry (e.g., Harbor). The artifact's layers<br />contain SQL migration files. Credentials are resolved from an<br />imagePullSecret in the bundle's namespace. |  |  |


#### MigrationSourceType

_Underlying type:_ _string_

MigrationSourceType discriminates between the supported source layers.

_Validation:_
- Enum: [ConfigMap OCIArtifact Git]

_Appears in:_
- [MigrationSource](#migrationsource)

| Field | Description |
| --- | --- |
| `ConfigMap` | SourceConfigMap reads SQL files from a Kubernetes ConfigMap. Each<br />data key is treated as a migration file; sorted lexicographically.<br />Recommended for module-internal migrations that ship with the<br />operator's GitOps tree.<br /> |
| `OCIArtifact` | SourceOCIArtifact (Phase 4+) pulls a signed OCI artifact containing<br />the SQL files. Production model: every release builds an artifact,<br />cosign-signs it, and the controller verifies before applying.<br /> |
| `Git` | SourceGit (Phase 4+) clones a Git ref and reads SQL from a path.<br />Useful for tenant-supplied migrations.<br /> |


#### MigrationStrategy

_Underlying type:_ _string_

MigrationStrategy controls how the MigrationController applies a bundle.

_Validation:_
- Enum: [versioned declarative pgroll-expand-contract]

_Appears in:_
- [MigrationBundleSpec](#migrationbundlespec)

| Field | Description |
| --- | --- |
| `versioned` | StrategyVersioned applies sequential numbered SQL files (e.g.<br />001_create_users.up.sql) inside a single transaction per file.<br />Compatible with golang-migrate file naming. Default.<br /> |
| `declarative` | StrategyDeclarative diffs the desired schema (declared as a single<br />CREATE TABLE / CREATE INDEX … bundle) against the live schema and<br />generates the ALTER chain. Mirrors SchemaHero / Atlas declarative<br />mode. Phase 6+ — not implemented in Phase 3.<br /> |
| `pgroll-expand-contract` | StrategyPgrollExpandContract uses pgroll's multi-version views to<br />stage column renames / drops without locking. Phase 6 implements<br />this via the embedded pgroll engine.<br /> |


#### MixedVersionPolicy

_Underlying type:_ _string_

MixedVersionPolicy controls how the SchemaDefinition reconciler
behaves when SchemaSelector matches multiple DatabaseSchemas whose
observed shapes diverge from each other (i.e. some tenants are
ahead of others).

Refuse (default) is the audit-grade choice: surface the drifted
schemas in Status.DriftedSchemas and emit no bundle until either
the schemas converge or the operator explicitly overrides via
MostBehindWins. No silent papering-over of tenant divergence.

MostBehindWins relaxes the gate: the reconciler picks the schema
that is farthest from desired, emits a bundle whose SQL brings it
to desired, and lets the runner's idempotent UPSERT semantics
no-op ahead-of-target tenants. Use only when fan-out drift is an
expected steady state (e.g. canary roll-out across tiers).

_Validation:_
- Enum: [Refuse MostBehindWins]

_Appears in:_
- [SchemaDefinitionSpec](#schemadefinitionspec)

| Field | Description |
| --- | --- |
| `Refuse` | MixedVersionPolicyRefuse halts the reconciler when fanned-out<br />DatabaseSchemas diverge. Default.<br /> |
| `MostBehindWins` | MixedVersionPolicyMostBehindWins emits a bundle keyed off the<br />most-behind schema; idempotent SQL handles ahead-tenants.<br /> |


#### PlanApprovalMode

_Underlying type:_ _string_

PlanApprovalMode is the effective approval mode resolved from the
matched SchemaPolicies. Stored in MigrationPlanStatus for
observability; see SchemaPolicy.Spec.PlanApproval for the source
field.

_Validation:_
- Enum: [Auto Manual]

_Appears in:_
- [MigrationPlanStatus](#migrationplanstatus)
- [SchemaPolicySpec](#schemapolicyspec)

| Field | Description |
| --- | --- |
| `Auto` | PlanApprovalAuto auto-flips spec.approved=true when the plan is<br />created (and lint passes). The controller bumps spec.approved<br />itself; operators don't see a pause.<br /> |
| `Manual` | PlanApprovalManual leaves spec.approved=false on plan creation.<br />A human or CI principal must explicitly patch the plan<br />(kubectl patch, keystonectl plan approve, GitOps) before<br />executions fire.<br /> |


#### PlannedStatement



PlannedStatement is one DDL statement the controller intends to execute,
surfaced for review before MigrationExecutions are created.



_Appears in:_
- [MigrationPlanSpec](#migrationplanspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `file` _string_ | File is the source filename the statement came from. |  |  |
| `index` _integer_ | Index is the 1-indexed statement number within File. |  | Minimum: 1 <br /> |
| `sql` _string_ | SQL is the rendered statement text (truncated to 4 KiB to keep<br />the CR size manageable; full SQL is in the source ConfigMap). |  | MaxLength: 4096 <br /> |


#### PolicyApprovalState



PolicyApprovalState is one entry in ApprovalSummary.Policies.



_Appears in:_
- [ApprovalSummary](#approvalsummary)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schemaPolicy` _string_ | SchemaPolicy is the owning SchemaPolicy.Name. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `approvalPolicy` _string_ | ApprovalPolicy is the nested ApprovalPolicy.Name within<br />SchemaPolicy.spec.approvalPolicies. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `applied` _boolean_ | Applied is true when AppliesWhen fired against this bundle.<br />When false, Required / Received / Satisfied carry no meaning —<br />the entry is listed for visibility only. |  |  |
| `required` _integer_ | Required is the count of distinct approvers the policy demands. |  |  |
| `received` _integer_ | Received is the count of distinct valid approvers counted:<br />those whose annotation group appears in FromGroups, minus the<br />author when DisallowSelfApproval is set. |  |  |
| `approvers` _string array_ | Approvers lists the approver-ids that counted toward Received,<br />sorted. Useful for audit trails and kubectl describe. |  | MaxItems: 32 <br /> |
| `satisfied` _boolean_ | Satisfied is Received >= Required for policies where Applied is<br />true. Always false when Applied is false. |  |  |
| `violation` _string_ | Violation is a human-readable diagnostic when Satisfied=false:<br />what was expected, what was received. Empty on satisfaction. |  | MaxLength: 512 <br /> |


#### ProductAPIRoute



ProductAPIRoute describes one external route the product exposes.



_Appears in:_
- [ProductDefinitionSpec](#productdefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `path` _string_ | Path is the public URL path (e.g. "/api/v1/crm"). |  | MaxLength: 512 <br />Required: \{\} <br /> |
| `upstreamService` _string_ | UpstreamService is the in-cluster Service the route forwards to. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `upstreamPort` _integer_ | UpstreamPort is the Service port number. | 80 | Maximum: 65535 <br />Minimum: 1 <br /> |


#### ProductDatabase



ProductDatabase describes one logical database the product needs.
A product may declare multiple databases (e.g. a core database
plus a separate financials database).



_Appears in:_
- [ProductDefinitionSpec](#productdefinitionspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ | Key is a stable identifier the product code uses to refer to<br />this database (e.g. "core", "billing"). Required so the controller<br />can name child LogicalDatabase resources predictably. |  | MaxLength: 32 <br />Pattern: `^[a-z][a-z0-9_-]*$` <br />Required: \{\} <br /> |
| `nameTemplate` _string_ | Name is the actual PostgreSQL database name template. Tokens<br />\{\{tenantID\}\} and \{\{productSlug\}\} are substituted by the<br />TenancyController at instantiation. Example: "example_suite_\{\{tenantID\}\}". |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `providerRef` _string_ | ProviderRef pins the DatabaseProvider this database lands on. A<br />product instance with multiple databases may spread across<br />providers; it's a deliberate decision the product author makes. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `isolation` _[IsolationMode](#isolationmode)_ | Isolation overrides the product-level default for this database.<br />Use to silo PCI-relevant data (billing) while keeping the rest in<br />bridge mode. |  | Enum: [pool bridge silo] <br /> |
| `schemas` _[ProductSchema](#productschema) array_ | Schemas declares the schemas that should exist within this<br />database. Each becomes a DatabaseSchema CR. |  | MaxItems: 128 <br /> |
| `extensions` _string array_ | Extensions to install in the database. Same allow-list semantics<br />as LogicalDatabase.spec.extensions. |  | MaxItems: 32 <br /> |


#### ProductDefinition



ProductDefinition is the platform-author's blueprint for a sellable
product. Cluster-scoped because products are platform-wide.
Replaces the iam_products row in the legacy db-provisioner.



_Appears in:_
- [ProductDefinitionList](#productdefinitionlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `ProductDefinition` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ProductDefinitionSpec](#productdefinitionspec)_ |  |  |  |
| `status` _[ProductDefinitionStatus](#productdefinitionstatus)_ |  |  |  |


#### ProductDefinitionList



ProductDefinitionList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `ProductDefinitionList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[ProductDefinition](#productdefinition) array_ |  |  |  |


#### ProductDefinitionSpec



ProductDefinitionSpec is the platform-author's blueprint for a
product. Cluster-scoped because products are platform infrastructure.



_Appears in:_
- [ProductDefinition](#productdefinition)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `slug` _string_ | Slug is the URL-safe product identifier. Used in API paths,<br />metrics labels, audit logs. |  | MaxLength: 63 <br />Pattern: `^[a-z][a-z0-9-]*$` <br />Required: \{\} <br /> |
| `displayName` _string_ | DisplayName is the human-friendly label. |  | MaxLength: 128 <br />Required: \{\} <br /> |
| `defaultIsolation` _[IsolationMode](#isolationmode)_ | DefaultIsolation is the isolation applied to ProductDatabases that<br />don't override it. Defaults to bridge — schema-per-tenant, the<br />industry sweet spot. | bridge | Enum: [pool bridge silo] <br /> |
| `databases` _[ProductDatabase](#productdatabase) array_ | Databases declares everything storage-related the product needs. |  | MaxItems: 8 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `lifecycleHooks` _[LifecycleHook](#lifecyclehook) array_ | LifecycleHooks fire on tenant lifecycle transitions. Phase 4.1<br />wires real HTTP delivery; Phase 4 surfaces intent only. |  | MaxItems: 8 <br /> |
| `apiRoutes` _[ProductAPIRoute](#productapiroute) array_ | APIRoutes declares the APISIX routes the product exposes. Phase<br />4.1 turns these into actual APISIX upstreams + routes; Phase 4<br />records them in status only. |  | MaxItems: 64 <br /> |


#### ProductDefinitionStatus



ProductDefinitionStatus tracks observability rollups.



_Appears in:_
- [ProductDefinition](#productdefinition)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state. |  |  |
| `instanceCount` _integer_ | InstanceCount is the number of ProductInstances currently<br />referencing this definition. |  |  |


#### ProductInstance



ProductInstance represents one tenant's subscription to a product.
Replaces the iam_product_instances row in the legacy db-provisioner.
The TenancyController reconciles it through the lifecycle phases by
creating child LogicalDatabase + DatabaseSchema CRs.



_Appears in:_
- [ProductInstanceList](#productinstancelist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `ProductInstance` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ProductInstanceSpec](#productinstancespec)_ |  |  |  |
| `status` _[ProductInstanceStatus](#productinstancestatus)_ |  |  |  |


#### ProductInstanceList



ProductInstanceList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `ProductInstanceList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[ProductInstance](#productinstance) array_ |  |  |  |


#### ProductInstancePhase

_Underlying type:_ _string_

ProductInstancePhase is the coarse lifecycle state for a tenant
instance of a product. Mirrors the legacy db-provisioner state machine
but graduated into a CR.

_Validation:_
- Enum: [Pending Provisioning Migrating ConfiguringAuth RegisteringRoutes Active Failed Deactivating Deactivated]

_Appears in:_
- [ProductInstanceStatus](#productinstancestatus)

| Field | Description |
| --- | --- |
| `Pending` |  |
| `Provisioning` |  |
| `Migrating` |  |
| `ConfiguringAuth` |  |
| `RegisteringRoutes` |  |
| `Active` |  |
| `Failed` |  |
| `Deactivating` |  |
| `Deactivated` |  |


#### ProductInstanceSpec



ProductInstanceSpec describes one tenant's instance of a product.



_Appears in:_
- [ProductInstance](#productinstance)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tenantID` _string_ | TenantID is the UUID identifying the tenant. Cross-referenced<br />against Falcon-ID's tenant directory; the controller does NOT<br />validate the UUID exists there (that's the orchestrator's<br />responsibility upstream). |  | MaxLength: 36 <br />Pattern: `^[0-9a-fA-F]\{8\}-[0-9a-fA-F]\{4\}-[0-9a-fA-F]\{4\}-[0-9a-fA-F]\{4\}-[0-9a-fA-F]\{12\}$` <br />Required: \{\} <br /> |
| `tenantSlug` _string_ | TenantSlug is a human-readable tenant identifier surfaced in audit<br />logs and dashboards. Optional; many SaaS shops use the email<br />domain or organisation name. |  | MaxLength: 63 <br /> |
| `productRef` _string_ | ProductRef is the ProductDefinition.metadata.name. Cluster-scoped<br />reference (ProductDefinition is cluster-scoped). |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `isolationOverride` _[IsolationMode](#isolationmode)_ | IsolationOverride forces a specific isolation mode for this<br />instance, regardless of the product's defaultIsolation. Use to<br />silo a high-compliance customer in an otherwise pool/bridge<br />product. Empty = use the product's default per-database setting. |  | Enum: [pool bridge silo] <br /> |
| `region` _string_ | Region pins the instance to a region (matches<br />ClusterRegistration.spec.region). Used by the RolloutController<br />(Phase 7) for region-scoped rollouts and by<br />data-residency policies. |  | MaxLength: 63 <br /> |
| `deletionPolicy` _string_ | DeletionPolicy controls what happens to the underlying databases<br />when this CR is deleted. Defaults to Retain — customer data is<br />never destroyed by an accidental kubectl delete. | Retain | Enum: [Retain Delete] <br /> |


#### ProductInstanceStatus



ProductInstanceStatus reports state machine progress + child resources.



_Appears in:_
- [ProductInstance](#productinstance)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `phase` _[ProductInstancePhase](#productinstancephase)_ | Phase is the coarse state. ArgoCD health checks read this field. |  | Enum: [Pending Provisioning Migrating ConfiguringAuth RegisteringRoutes Active Failed Deactivating Deactivated] <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports per-step status:<br />  Ready             — Phase=Active and every child resource is Ready<br />  DatabasesReady    — every InstanceDatabase has Ready LogicalDatabase<br />  SchemasReady      — every DatabaseSchema is Ready<br />  MigrationsApplied — initial migrations from product schemas have<br />                      all reached Succeeded<br />  AuthConfigured    — SpiceDB tuples written (Phase 4.1)<br />  RoutesRegistered  — APISIX routes upserted (Phase 4.1) |  |  |
| `databases` _[InstanceDatabase](#instancedatabase) array_ | Databases lists child LogicalDatabase + DatabaseSchema refs.<br />Maintained on every reconcile; clearing it requires the deletion<br />flow to remove the children first. |  | MaxItems: 8 <br /> |
| `appliedRoutes` _string array_ | AppliedRoutes are the APISIX route IDs the controller created.<br />Empty in Phase 4 (HTTP integration is Phase 4.1). |  | MaxItems: 64 <br /> |
| `lastTransitionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastTransitionTime is when Phase last changed. |  |  |


#### ProductSchema



ProductSchema describes one schema within a ProductDatabase.



_Appears in:_
- [ProductDatabase](#productdatabase)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `key` _string_ | Key is a stable identifier (e.g. "crm", "billing"). |  | MaxLength: 32 <br />Required: \{\} <br /> |
| `nameTemplate` _string_ | NameTemplate is the schema name template (same tokens as<br />ProductDatabase.NameTemplate). |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `migrationBundleRef` _string_ | MigrationBundleRef is the MigrationBundle name that ships the<br />initial DDL for this schema. Optional — schemas without an<br />initial migration just become empty CREATE SCHEMA targets. |  | MaxLength: 253 <br /> |


#### RenameColumnOp



RenameColumnOp renames a column. Expand: ADD new + COPY + TRIGGER for
dual-write; Contract: DROP TRIGGER + DROP old.



_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `from` _string_ | From is the existing column name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `to` _string_ | To is the new column name. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |


#### RolloutPolicy



RolloutPolicy declares an ordered staged-rollout pipeline shared
across MigrationBundles via spec.rolloutPolicyRef. Cluster-scoped.



_Appears in:_
- [RolloutPolicyList](#rolloutpolicylist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `RolloutPolicy` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[RolloutPolicySpec](#rolloutpolicyspec)_ |  |  |  |
| `status` _[RolloutPolicyStatus](#rolloutpolicystatus)_ |  |  |  |


#### RolloutPolicyList



RolloutPolicyList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `RolloutPolicyList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[RolloutPolicy](#rolloutpolicy) array_ |  |  |  |


#### RolloutPolicySpec



RolloutPolicySpec describes a reusable staged-rollout pipeline that
MigrationBundles can reference via spec.rolloutPolicyRef. Cluster-
scoped because policies are platform-wide governance.



_Appears in:_
- [RolloutPolicy](#rolloutpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `stages` _[RolloutStage](#rolloutstage) array_ | Stages is the ordered pipeline. Empty = error at admission<br />(use spec.source-only mode without a policy if you want<br />all-at-once fanout). |  | MaxItems: 8 <br />MinItems: 1 <br />Required: \{\} <br /> |


#### RolloutPolicyStatus



RolloutPolicyStatus tracks usage.



_Appears in:_
- [RolloutPolicy](#rolloutpolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state. |  |  |
| `matchingBundles` _integer_ | MatchingBundles is the count of MigrationBundles currently<br />referencing this policy. Surfaced for ops visibility — a policy<br />with zero matches is dead code. |  |  |


#### RolloutStage



RolloutStage describes one tier in a staged rollout pipeline. The
MigrationController processes stages in array order; advancement is
gated by exit criteria (all targets succeeded + soak elapsed +
optional approval).



_Appears in:_
- [RolloutPolicySpec](#rolloutpolicyspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name is a stable identifier for the stage. Used in<br />status.stageHistory keys and in the approval annotation<br />(keystone.hexxlock.io/approve-stage-<name>=true). |  | MaxLength: 63 <br />Pattern: `^[a-z][a-z0-9-]*$` <br />Required: \{\} <br /> |
| `schemaSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#labelselector-v1-meta)_ | SchemaSelector picks which DatabaseSchemas this stage targets,<br />among the bundle's already-matched set. Stages typically select<br />by tier: matchLabels: \{tier: canary\}. Empty selector matches<br />everything left over after prior stages — useful for a "rest"<br />stage at the end. |  |  |
| `clusterTierSelector` _[ClusterTier](#clustertier) array_ | ClusterTierSelector restricts the stage to schemas whose<br />LogicalDatabase.spec.clusterRef points at a ClusterRegistration<br />with one of these tiers. Empty = no cluster-tier restriction. |  | Enum: [canary free paid internal] <br />MaxItems: 4 <br /> |
| `parallelism` _integer_ | Parallelism caps how many MigrationExecutions can be<br />in-flight (Pending/Running/Expanding/Contracting) inside this<br />stage at once. Smaller = safer but slower. Default 1 — the<br />controller refuses to default to "all at once" because that's<br />the failure mode this whole CRD exists to prevent. | 1 | Maximum: 100 <br />Minimum: 1 <br /> |
| `soakDuration` _string_ | SoakDuration is how long the controller waits AFTER all stage<br />targets reach Succeeded before advancing to the next stage. Use<br />to give SLO/error-budget signals time to register before<br />promoting. Format: Go duration string (e.g. "10m", "1h"). | 10m | MaxLength: 16 <br /> |
| `requireApproval` _boolean_ | RequireApproval gates advancement to the NEXT stage on a<br />human acknowledgement. Operator sets the annotation<br />keystone.hexxlock.io/approve-stage-<this.Name>=true on the<br />MigrationBundle to release the gate. Default false — most<br />non-paid stages don't need it. | false |  |
| `abortOnFailure` _boolean_ | AbortOnFailure controls what happens when a MigrationExecution<br />in this stage fails. true (default) = freeze the rollout in<br />this stage; the operator must investigate and resolve before<br />advancement. false = continue to the next stage despite<br />failures (rare; useful for best-effort cleanup migrations). | true |  |


#### SchemaDefinition



SchemaDefinition declares the desired shape of a DatabaseSchema. The
controller diffs live state against this spec and auto-generates a
MigrationBundle with the necessary DDL. Atlas-parity DX for
declarative schema authoring.

Phase 10.1 ships the CRD only. Phase 10.2 ships the differ.
Phase 10.3 ships the reconciler.



_Appears in:_
- [SchemaDefinitionList](#schemadefinitionlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `SchemaDefinition` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SchemaDefinitionSpec](#schemadefinitionspec)_ |  |  |  |
| `status` _[SchemaDefinitionStatus](#schemadefinitionstatus)_ |  |  |  |


#### SchemaDefinitionList



SchemaDefinitionList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `SchemaDefinitionList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[SchemaDefinition](#schemadefinition) array_ |  |  |  |


#### SchemaDefinitionSpec



SchemaDefinitionSpec is the declarative desired-state for one schema
or for a label-selected fan-out of schemas.

Exactly one of SchemaRef (singular) and SchemaSelector (fan-out)
must be set. Mixing both is rejected by admission. SchemaRef stays
the default for single-tenant definitions; SchemaSelector enables
one SchemaDefinition to drive every DatabaseSchema in a tenant pool
(the realm-DB pattern in example-service, business-module DBs in example-suite,
etc.) without per-tenant CR boilerplate.



_Appears in:_
- [SchemaDefinition](#schemadefinition)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schemaRef` _string_ | SchemaRef is the DatabaseSchema CR this definition applies to<br />(same namespace). Mutually exclusive with SchemaSelector. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `schemaSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#labelselector-v1-meta)_ | SchemaSelector matches every DatabaseSchema in the same namespace<br />whose labels satisfy the selector. The reconciler resolves all<br />matched schemas, computes a per-schema diff, and (subject to<br />MixedVersionPolicy) emits one MigrationBundle whose<br />schemaSelector mirrors this one — the bundle's existing fan-out<br />at apply time then runs against every matched schema.<br />An empty selector (\{\}) matches everything in-namespace and is<br />rejected by admission to prevent foot-guns; specify at least one<br />matchLabels entry. Mutually exclusive with SchemaRef. |  |  |
| `mixedVersionPolicy` _[MixedVersionPolicy](#mixedversionpolicy)_ | MixedVersionPolicy governs reconciler behaviour when<br />SchemaSelector matches multiple schemas whose observed shapes<br />diverge. Ignored when SchemaRef (singular) is used. | Refuse | Enum: [Refuse MostBehindWins] <br /> |
| `tables` _[DesiredTable](#desiredtable) array_ | Tables is the ordered list of desired tables. |  | MaxItems: 1024 <br />MinItems: 1 <br />Required: \{\} <br /> |
| `enums` _[DesiredEnum](#desiredenum) array_ | Enums are PostgreSQL enum types. Created before tables that<br />reference them. Drop requires AllowDestructive. |  | MaxItems: 256 <br /> |
| `extensions` _[DesiredExtension](#desiredextension) array_ | Extensions are PostgreSQL extensions the schema depends on.<br />Created before everything else, since types and default<br />expressions can come from them. |  | MaxItems: 64 <br /> |
| `sequences` _[DesiredSequence](#desiredsequence) array_ | Sequences are PostgreSQL sequences. Created before tables that<br />use them as DEFAULT values. |  | MaxItems: 256 <br /> |
| `views` _[DesiredView](#desiredview) array_ | Views are PostgreSQL views. Created after all referenced tables. |  | MaxItems: 256 <br /> |
| `functions` _[DesiredFunction](#desiredfunction) array_ | Functions are PostgreSQL functions/procedures. Created after all<br />referenced types and tables. |  | MaxItems: 256 <br /> |
| `materializedViews` _[DesiredMaterializedView](#desiredmaterializedview) array_ | MaterializedViews are PostgreSQL materialized views. Created<br />after all referenced tables. Support indexes for query perf. |  | MaxItems: 128 <br /> |
| `policies` _[DesiredPolicy](#desiredpolicy) array_ | Policies are PostgreSQL row-level security policies. Requires<br />enableRLS=true on the target table. |  | MaxItems: 256 <br /> |
| `triggers` _[DesiredTrigger](#desiredtrigger) array_ | Triggers are PostgreSQL trigger bindings. The referenced<br />function must exist in spec.functions or already in the DB. |  | MaxItems: 256 <br /> |
| `allowDestructive` _boolean_ | AllowDestructive gates whether the differ may emit<br />DROP TABLE / DROP COLUMN operations. Default false: the<br />SchemaDefinitionReconciler refuses to plan destructive changes<br />silently. Operators flip this on a per-SchemaDefinition basis<br />when they've reviewed the diff and decided the drop is safe. | false |  |
| `defaultPrivileges` _[DesiredDefaultPrivilege](#desireddefaultprivilege) array_ | DefaultPrivileges declares per-role `ALTER DEFAULT PRIVILEGES`<br />that should hold against this schema. Every entry expands at<br />reconcile time to:<br />  ALTER DEFAULT PRIVILEGES FOR ROLE <ForRole><br />    IN SCHEMA <Schema OR sd.targetSchema><br />    GRANT <Privileges> ON <ObjectType> TO <ToRole>;<br />Why this exists (gap surfaced 2026-05-12 on example-service migration<br />234): the operator-emitted bundles run CREATE TABLE as the<br />keystone_admin role. Pre-Keystone default-privileges were set<br />for the `postgres` role, so any keystone_admin-created table<br />silently dropped out of the falcon_id_app grant chain. The<br />BackchannelLogoutDrainer spammed SQLSTATE 42501 every 5s until<br />a hand-authored migration bundle re-pointed ALTER DEFAULT<br />PRIVILEGES at keystone_admin. With DefaultPrivileges declared<br />here, that recovery is no longer hand-authored — the SD<br />reconciler emits the ALTER DEFAULT PRIVILEGES statement as part<br />of the bundle so every future keystone_admin-created table<br />inherits the grant chain automatically. |  | MaxItems: 64 <br /> |
| `applyStrategy` _string_ | ApplyStrategy selects how the generated MigrationBundle is<br />written: "versioned" (raw ALTER statements) or<br />"pgroll-expand-contract" (zero-downtime for drop/rename/alter).<br />Default "versioned" for greenfield schemas where locks are OK. | versioned | Enum: [versioned pgroll-expand-contract] <br /> |
| `policyRef` _string_ | PolicyRef is the SchemaPolicy the reconciler stamps onto the<br />auto-generated MigrationBundle. When empty, the default<br />"production-safety" policy is applied — this matches the value<br />used by every hand-authored bundle in the GitOps repository<br />and ensures the downstream MigrationBundle controller has a<br />policy to look up (without a policyRef, the bundle pipeline<br />stalls before creating a MigrationPlan). |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br /> |
| `cleanup` _[CleanupPolicy](#cleanuppolicy)_ | Cleanup configures the per-SD TTL policy applied to terminal<br />MigrationBundle and MigrationExecution CRs owned by this<br />SchemaDefinition. Without this controller running, terminal CRs<br />accumulate without bound — the 2026-05-08 incident demonstrated<br />the failure mode at 182 954 AuditEntries / 4.7 GB etcd / 17 h<br />apiserver outage. (AuditEntry has its own off-cluster archive<br />path under spec.archive on AuditLog; this Cleanup field covers<br />the operator's own transient bundles + executions whose payload<br />stays on-cluster.)<br />Nil = use the operator-wide defaults (24 h success / 7 d<br />failure). Non-terminal CRs (Pending / Running / Expanding /<br />Aborting / RollingBack) are NEVER deleted regardless of age —<br />the controller only acts on Succeeded / Failed / Aborted /<br />RolledBack / RollbackFailed phases. The latest succeeded<br />MigrationBundle and the latest succeeded MigrationExecution per<br />(SD, schema) tuple are always retained so the SD reconciler can<br />re-emit decisions and operators can read "what last shipped".<br />Behind the operator-level --enable-cr-ttl feature flag (default<br />off for first release) so existing clusters opt in deliberately. |  |  |


#### SchemaDefinitionStatus



SchemaDefinitionStatus reports the observed diff + auto-generated
MigrationBundle reference.



_Appears in:_
- [SchemaDefinition](#schemadefinition)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions:<br />  Ready           — schema matches desired state<br />  Inspected       — live inspection completed successfully<br />  DiffComputed    — differ produced an operation plan (True with<br />                    reason=NoDrift means the plan was empty)<br />  BundleGenerated — a MigrationBundle was emitted to Apply the<br />                    diff; tracks CurrentBundleRef, so False<br />                    whenever that field is empty<br />The last three are pipeline stages, re-stated on every terminal<br />path rather than latched, so all four always describe the same<br />reconcile. See ConditionTypeInspected and friends. |  |  |
| `currentBundleRef` _string_ | CurrentBundleRef is the auto-generated MigrationBundle that's<br />currently applying the diff. Empty when the schema matches<br />desired state (Ready=True). |  | MaxLength: 253 <br /> |
| `pendingOperations` _integer_ | PendingOperations counts the diff size from the last reconcile.<br />0 means the schema is at desired state. When SchemaSelector<br />fan-out is used this reflects the size of the emitted bundle's<br />op set (i.e. the most-behind schema's diff under MostBehindWins<br />policy, or the lockstep diff under Refuse with all schemas in<br />agreement). |  |  |
| `lastDiffTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | LastDiffTime is when the differ last ran. |  |  |
| `matchedSchemas` _integer_ | MatchedSchemas is the count of DatabaseSchemas the reconciler<br />resolved on the last pass. 1 for SchemaRef, N for SchemaSelector.<br />Surfaces fan-out width on `kubectl get sd`. |  |  |
| `fingerprint` _string_ | Fingerprint is a deterministic hash of the SD's desired-state<br />spec (tables + columns + indexes + functions + ... — same<br />inputs the differ consumes). Recomputed on every reconcile;<br />changes whenever the operator edits the spec. The SD reconciler<br />stamps this on every emitted MigrationBundle as the annotation<br />`keystone.hexxlock.io/sd-fingerprint`; the bundle runner copies<br />it onto DatabaseSchema.Status.LastAppliedFingerprint when a<br />MigrationExecution succeeds for that schema.<br />SDK consumers (pkg/sdk/keystone.WaitSchemaFingerprint) compare<br />this against per-schema LastAppliedFingerprint to verify<br />convergence in an identity-based, race-free way.<br />Empty when the spec has never been hashed (very first reconcile<br />of a freshly-applied SD). |  | MaxLength: 64 <br /> |
| `driftedSchemas` _[DriftedSchema](#driftedschema) array_ | DriftedSchemas lists DatabaseSchemas whose observed shapes<br />diverge from the lockstep set when SchemaSelector + MixedVersion<br />Policy=Refuse is in effect. Empty under MostBehindWins (the<br />reconciler proceeds rather than recording divergence) and empty<br />under SchemaRef (no fan-out, no divergence to surface). |  | MaxItems: 1024 <br /> |


#### SchemaPolicy



SchemaPolicy declares the rules MigrationBundles must satisfy. Cluster-
scoped because policies are platform-wide governance.



_Appears in:_
- [SchemaPolicyList](#schemapolicylist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `SchemaPolicy` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SchemaPolicySpec](#schemapolicyspec)_ |  |  |  |
| `status` _[SchemaPolicyStatus](#schemapolicystatus)_ |  |  |  |


#### SchemaPolicyList



SchemaPolicyList is the list wrapper required by client-go.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `SchemaPolicyList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[SchemaPolicy](#schemapolicy) array_ |  |  |  |


#### SchemaPolicySpec



SchemaPolicySpec describes the rules that apply to MigrationBundles
matching this policy. Multiple policies may apply to the same bundle;
when they do, the strictest setting wins per-rule.



_Appears in:_
- [SchemaPolicy](#schemapolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#labelselector-v1-meta)_ | TargetSelector picks the MigrationBundles this policy governs.<br />Match is by metadata.labels using standard label selectors. |  | Required: \{\} <br /> |
| `minLintLevel` _[LintLevel](#lintlevel)_ | MinLintLevel is the lowest severity that fails admission. Default<br />is `error` — warnings and notices surface in status but do not<br />block. Set to `warning` to gate stricter, or `notice` for the<br />strictest possible. | error | Enum: [error warning notice] <br /> |
| `allowedStrategies` _string array_ | AllowedStrategies is the set of MigrationBundle.spec.strategy<br />values this policy permits. Empty means any. Use to forbid raw<br />`versioned` migrations on production schemas, requiring<br />`pgroll-expand-contract` for zero-downtime guarantees. |  | MaxItems: 8 <br /> |
| `maxStatementsPerMigration` _integer_ | MaxStatementsPerMigration caps a single migration file's statement<br />count. Long migrations correlate with long-running locks; the cap<br />pushes operators toward smaller, reversible chunks. | 50 | Maximum: 10000 <br />Minimum: 1 <br /> |
| `requireApproval` _string array_ | RequireApproval lists annotations that the admission webhook must<br />see on the MigrationBundle. Each entry is the annotation key; the<br />value of the annotation must match the GitLab CODEOWNERS approval<br />signature posted by CI. Empty list = no approval gate (suitable<br />for canary tier).<br />Deprecated: Use ApprovalPolicies for N-of-M / role-based / risk-<br />tier-conditional approvals. RequireApproval is kept for backward<br />compatibility and will be removed in v1beta1. |  | MaxItems: 8 <br /> |
| `approvalPolicies` _[ApprovalPolicy](#approvalpolicy) array_ | ApprovalPolicies is the N-of-M approval workflow. Every matched<br />MigrationBundle must satisfy each policy in turn before<br />admission accepts it. A bundle with zero policies bypasses this<br />gate — suitable for canary tiers; production tiers ship at<br />least one policy.<br />Each policy declares:<br />  - RequiredApprovers — count of distinct approvers needed<br />  - FromGroups        — identity groups that may satisfy the count<br />  - AppliesWhen       — conditional expression on bundle metadata<br />                        (label selector + lint-finding threshold)<br />Satisfaction evidence lives under MigrationBundle.metadata.annotations<br />with keys of the form:<br />  keystone.hexxlock.io/approval.<policy-name>.<approver-id>=<token><br />Tokens are short-lived signed JWTs issued by the Keystone<br />approval-issuer webhook (Phase 12.2). Today we accept raw<br />CODEOWNERS signatures; JWT issuance lands with the UI. |  | MaxItems: 16 <br /> |
| `blockedKeywords` _string array_ | BlockedKeywords is a list of SQL keywords the policy refuses to<br />permit. Common values: DROP TABLE, DROP COLUMN, TRUNCATE,<br />DROP DATABASE. Case-insensitive substring match. Bypass is by<br />adding `keystone.hexxlock.io/policy-override: true` annotation |  | MaxItems: 32 <br /> |
| `integrity` _[IntegrityPolicy](#integritypolicy)_ | Integrity opts in to keystone.sum-based directory integrity<br />enforcement for matched bundles. Default (nil) is advisory — the<br />controller still verifies any committed sum, but a missing sum<br />does not block admission. See docs/adrs/0015 for rollout guidance. |  |  |
| `devDatabaseRef` _string_ | DevDatabaseRef is an optional reference to a DatabaseProvider<br />that hosts a dev database for semantic migration analysis. When<br />set, the MigrationBundle reconciler replays migrations against<br />an ephemeral schema in this database to catch semantic errors<br />that static analysis misses (invalid SQL, type mismatches, FK<br />reference errors, etc.). This is Keystone's equivalent of<br />Atlas's --dev-url pattern.<br />The referenced DatabaseProvider should point to a dedicated dev<br />database — NOT a production database. The reconciler creates<br />temporary schemas (ks_dev_*) and drops them after each lint run. |  | MaxLength: 253 <br /> |
| `planApproval` _[PlanApprovalMode](#planapprovalmode)_ | PlanApproval selects the Plan-Gate-Apply mode for matched<br />MigrationBundles. Auto (default) flips each plan's<br />spec.approved=true automatically so executions fire without a<br />human in the loop. Manual leaves plans at spec.approved=false<br />until a reviewer explicitly approves — the canonical production<br />gate. When multiple matched policies disagree, Manual wins<br />(strictest rule). See docs/adrs/0016 for the approval trust model<br />and docs/adrs/0017 for the Plan-Gate-Apply lifecycle. |  | Enum: [Auto Manual] <br /> |
| `expressions` _[CELRule](#celrule) array_ | Expressions is a list of CEL (Common Expression Language) rules<br />the webhook evaluates at admission time against every matched<br />MigrationBundle. Each rule's expression must evaluate to a bool;<br />true means the rule fires (warning or rejection, depending on<br />severity). See docs/adrs/0023 for the activation variable shape<br />and examples.<br />CEL rules coexist with hardcoded fields above — they don't<br />replace AllowedStrategies / BlockedKeywords / etc. Use CEL for<br />policy dimensions Keystone doesn't model as first-class fields<br />("forbid prod bundles that include DROP COLUMN AND have no<br />approval annotation", "reject any bundle missing a Jira ticket<br />label", etc.). |  | MaxItems: 32 <br /> |


#### SchemaPolicyStatus



SchemaPolicyStatus tracks how often the policy fires.



_Appears in:_
- [SchemaPolicy](#schemapolicy)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions reports the current state. |  |  |
| `matchingBundles` _integer_ | MatchingBundles is the count of MigrationBundles currently matched<br />by this policy. Surfaced for ops visibility — a policy with zero<br />matches is dead code. |  |  |


#### SchemaPrivilegeDefault



SchemaPrivilegeDefault declares the default privileges granted to roles
for objects subsequently created in the schema. Mirrors PostgreSQL
`ALTER DEFAULT PRIVILEGES`. Mutating after creation requires the
controller to re-apply ALTER DEFAULT PRIVILEGES; existing objects are
not retroactively re-granted.



_Appears in:_
- [DatabaseSchemaSpec](#databaseschemaspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `role` _string_ | Role is the role being granted to (or revoked from). |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |
| `objectType` _string_ | ObjectType is the PostgreSQL object class the defaults apply to. |  | Enum: [tables sequences functions types schemas] <br />Required: \{\} <br /> |
| `privileges` _string array_ | Privileges is the list of privileges to grant. Empty means "revoke<br />all defaults" — distinct from omitting the entry, which leaves<br />existing defaults untouched. |  | MaxItems: 16 <br /> |


#### SchemaProgress



SchemaProgress is one entry in MigrationBundleStatus.SchemaProgress
— the observed state of a single (bundle, version, schema) triple's
MigrationExecution. Operators read this for per-tenant triage when
a bundle fans out across dozens of tenant databases.



_Appears in:_
- [MigrationBundleStatus](#migrationbundlestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schemaRef` _string_ | SchemaRef is the DatabaseSchema's metadata.name. |  | MaxLength: 253 <br />Required: \{\} <br /> |
| `tenantID` _string_ | TenantID carries the value of the DatabaseSchema's<br />keystone.hexxlock.io/tenant-id label when set. Empty when the<br />schema was not labelled. Surfaced so dashboards can group<br />rollouts by tenant without joining against ProductInstance. |  | MaxLength: 253 <br /> |
| `executionRef` _string_ | ExecutionRef is the MigrationExecution.metadata.name associated<br />with this (bundle, version, schema) triple. Empty when the<br />execution has not yet been created (throttled, awaiting earlier<br />stage). |  | MaxLength: 253 <br /> |
| `phase` _string_ | Phase mirrors the MigrationExecution.status.phase at observation<br />time. Empty when no Execution exists yet. |  | MaxLength: 32 <br /> |
| `message` _string_ | Message is a short diagnostic — the first line of the<br />execution's Ready condition message. Empty when the execution<br />is in a nominal state. |  | MaxLength: 512 <br /> |
| `startedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | StartedAt is the execution's StartTime (when Apply began).<br />Empty until the execution enters Running / Expanding. |  |  |
| `completedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | CompletedAt is the execution's CompletionTime (when Succeeded,<br />Failed, or Aborted was set). Empty while in-flight. |  |  |


#### SchemaSnapshot



SchemaSnapshot is an immutable record of a DatabaseSchema's
applied-migration state at a point in time. See the type's Spec
comment for the restore workflow.



_Appears in:_
- [SchemaSnapshotList](#schemasnapshotlist)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `SchemaSnapshot` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SchemaSnapshotSpec](#schemasnapshotspec)_ |  |  |  |
| `status` _[SchemaSnapshotStatus](#schemasnapshotstatus)_ |  |  |  |


#### SchemaSnapshotList



SchemaSnapshotList is the list wrapper.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `keystone.hexxlock.io/v1alpha1` | | |
| `kind` _string_ | `SchemaSnapshotList` | | |
| `kind` _string_ | Kind is a string value representing the REST resource this object represents.<br />Servers may infer this from the endpoint the client submits requests to.<br />Cannot be updated.<br />In CamelCase.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds |  |  |
| `apiVersion` _string_ | APIVersion defines the versioned schema of this representation of an object.<br />Servers should convert recognized schemas to the latest internal value, and<br />may reject unrecognized values.<br />More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources |  |  |
| `metadata` _[ListMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#listmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `items` _[SchemaSnapshot](#schemasnapshot) array_ |  |  |  |


#### SchemaSnapshotSpec



SchemaSnapshotSpec captures a point-in-time record of a
DatabaseSchema's applied-migration state. Combined with the ability
to replay a list of MigrationBundles at the recorded versions, it's
Keystone's backup/restore primitive — the minimum needed to claim
Red Hat Operator Capability Level 3.

Operator UX is:

  1. Before a risky change, declare a SchemaSnapshot. The reconciler
     reads schema_migrations + the inspector's drift fingerprint
     and freezes them into the CR's status.

  2. If the risky change goes wrong, create a MigrationBundle whose
     source is the DIFFerence between the snapshot and the current
     state — the declarative differ handles the inverse in Phase
     12. Meanwhile, operators can use snapshots as evidence for
     PCI/SOC 2 change-management audits ("this is what state X
     looked like before we ran migration Y").

Snapshots are immutable once captured. This is enforced by the
reconciler, which returns immediately for any snapshot already in
phase Captured or Failed — not by a webhook. Nothing in-tree writes
to a captured snapshot; new fields must still be added as +optional
so that archived snapshots stay admissible against a newer CRD.



_Appears in:_
- [SchemaSnapshot](#schemasnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schemaRef` _string_ | SchemaRef names the DatabaseSchema the snapshot captures. The<br />reconciler requires the referenced DatabaseSchema to be Ready<br />at capture time; transient schemas aren't snapshotted. |  | MaxLength: 253 <br />Pattern: `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` <br />Required: \{\} <br /> |
| `reason` _string_ | Reason is a human-readable description of why the snapshot was<br />taken. Surfaced in audit log entries and restore UIs. |  | MaxLength: 1024 <br /> |
| `retentionDays` _integer_ | RetentionDays is the floor before a future cleanup job may prune<br />this snapshot. Default 365. Set higher for regulatory holds. | 365 | Maximum: 36500 <br />Minimum: 1 <br /> |


#### SchemaSnapshotStatus



SchemaSnapshotStatus captures the observed state at snapshot time.
Once `phase=Captured`, every field here is immutable — subsequent
reconciles observe them as-is and do not mutate.



_Appears in:_
- [SchemaSnapshot](#schemasnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `observedGeneration` _integer_ | ObservedGeneration is the .metadata.generation last reconciled. |  |  |
| `phase` _string_ | Phase is the snapshot lifecycle state. |  | Enum: [Pending Capturing Captured Failed] <br /> |
| `capturedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | CapturedAt is when the snapshot was frozen. |  |  |
| `appliedMigrations` _[AppliedMigrationRecord](#appliedmigrationrecord) array_ | AppliedMigrations is the ordered list of (version, contentHash)<br />pairs from the target's schema_migrations table at capture<br />time. The order matches apply order — lexicographic version<br />ascending, consistent with golang-migrate. |  | MaxItems: 8192 <br /> |
| `fingerprint` _string_ | Fingerprint is the drift-inspector fingerprint at capture time.<br />Downstream verifiers compare against a fresh inspection to<br />detect drift since the snapshot. |  | MaxLength: 128 <br /> |
| `checksumSha256` _string_ | ChecksumSHA256 is the SHA-256 over a canonical encoding of<br />AppliedMigrations + Fingerprint. Immutability evidence:<br />downstream tooling can recompute and verify after restore. |  | MaxLength: 64 <br />Pattern: `^[0-9a-f]\{64\}$` <br /> |
| `erd` _string_ | ERD is a Mermaid erDiagram representation of the schema at<br />capture time. Rendered from the drift inspector's structural<br />snapshot — tables, columns, PKs, FKs, unique constraints.<br />HexxForge UI and GitLab markdown render this natively. |  | MaxLength: 1048576 <br /> |
| `structure` _[StructuralSnapshot](#structuralsnapshot)_ | Structure is the structured representation of the schema shape<br />— tables, columns, indexes, constraints — captured from the<br />drift inspector at the same instant as Fingerprint/ERD. Serves<br />as the authoritative artifact for snapshot-to-snapshot diff<br />(`keystonectl snapshot diff`). Populated alongside ERD; the ERD<br />stays for human-readable rendering and Structure for machine<br />analysis. |  |  |
| `autoSource` _string_ | AutoSource identifies the MigrationExecution that triggered<br />automatic capture. Empty for manually-created snapshots. Pattern:<br />"<bundle>@<version>". Useful for querying "find the snapshot<br />that recorded the post-apply state of bundle X version Y". |  | MaxLength: 320 <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#condition-v1-meta) array_ | Conditions follow Kubernetes conventions. |  |  |


#### SetNotNullOp



SetNotNullOp is the payload for Kind=set_not_null.



_Appears in:_
- [MigrationOperation](#migrationoperation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `column` _string_ | Column is the name of the column to transition to NOT NULL. The<br />column must already exist and must be nullable; admission<br />enforces neither — the engine errors with a clear diagnostic. |  | MaxLength: 63 <br />Pattern: `^[a-z_][a-z0-9_]*$` <br />Required: \{\} <br /> |


#### SnapshotColumn



SnapshotColumn captures one column's shape. DataType + UDTName are
both preserved because PG exposes both (data_type "integer" vs
udt_name "int4"); downstream tools may prefer one or the other.



_Appears in:_
- [SnapshotTable](#snapshottable)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the column. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `ordinal` _integer_ | Ordinal is the 1-based column position. Matches information_<br />schema.columns.ordinal_position so reordering is detectable. |  | Minimum: 1 <br /> |
| `dataType` _string_ | DataType is the information_schema.data_type value ("text",<br />"integer", "timestamp with time zone", etc.). |  | MaxLength: 64 <br /> |
| `udtName` _string_ | UDTName is the udt_name ("text", "int4", "timestamptz"). More<br />canonical than DataType for non-standard types. |  | MaxLength: 64 <br /> |
| `nullable` _boolean_ | Nullable is the is_nullable flag. |  |  |
| `default` _string_ | Default is the column_default expression, empty when no default. |  | MaxLength: 1024 <br /> |


#### SnapshotEnum



SnapshotEnum is one enum type and its labels.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the enum type. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `labels` _string array_ | Labels in PostgreSQL's sort order (pg_enum.enumsortorder), which<br />is the order the labels compare in, not the order they were<br />declared. Order is therefore semantic here — unlike a policy's<br />role list — and a reordered label list is a real change: it<br />silently reverses every ORDER BY and range comparison on the<br />column. |  | MaxItems: 1024 <br /> |


#### SnapshotExtension



SnapshotExtension is one extension installed into the schema.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the extension. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `schema` _string_ | Schema the extension is installed into. Recorded even though the<br />snapshot describes one schema, because the inspector reports it<br />and a mismatch is a signal the capture is not what it claims. |  | MaxLength: 63 <br /> |


#### SnapshotFunction



SnapshotFunction is one function or procedure, body included.

Functions are overloadable, so Name alone does not identify one —
(Name, Args) does. A reader that keys on the name will collapse an
overload set and can report a dropped overload as unchanged, the same
way keying a policy on its bare name collapses two tables' policies.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the function. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `args` _string_ | Args is the argument list as pg_get_function_arguments renders<br />it. Part of the identity, not a detail. |  | MaxLength: 4096 <br /> |
| `returns` _string_ | Returns is the result type as pg_get_function_result renders it. |  | MaxLength: 1024 <br /> |
| `language` _string_ | Language the function is written in: sql, plpgsql, c, … |  | MaxLength: 64 <br /> |
| `definition` _string_ | Definition is the function body verbatim.<br />Captured because the body is where a security-relevant change<br />hides: a SECURITY DEFINER helper, or the current_tenant() a<br />policy's USING clause calls, can be rewritten without touching<br />its signature. A snapshot that recorded only the signature would<br />report no change across exactly that edit. |  | MaxLength: 131072 <br /> |


#### SnapshotMaterializedView



SnapshotMaterializedView is one materialized view and its defining
SELECT.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the materialized view. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `definition` _string_ | Definition is the SELECT verbatim, as pg_matviews reports it. |  | MaxLength: 131072 <br /> |


#### SnapshotObjectDDL



SnapshotObjectDDL is the generic name+definition pair used for
indexes and constraints. Definition is the canonical PG-emitted
DDL (pg_get_indexdef / pg_get_constraintdef) so diffs can compare
byte-exact definitions without re-parsing.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the object. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `table` _string_ | Table is the object's parent table, empty for schema-level<br />objects. |  | MaxLength: 63 <br /> |
| `type` _string_ | Type discriminates the object: btree/hash/gin/... for indexes,<br />PRIMARY KEY/UNIQUE/CHECK/FOREIGN KEY/EXCLUDE for constraints. |  | MaxLength: 32 <br /> |
| `definition` _string_ | Definition is the canonical DDL. Used for byte-exact diff. |  | MaxLength: 4096 <br />Required: \{\} <br /> |


#### SnapshotPolicy



SnapshotPolicy is one row-level security policy as pg_policies
reports it. Together with SnapshotTable's RLSEnabled/RLSForced this
is the whole of a schema's tenant-isolation posture, which is why a
snapshot that omits it cannot serve as the change-management
evidence this type claims to be: dropping a policy, or flipping a
table to NO FORCE, left the captured structure byte-identical.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the policy. Unique per table, not per schema. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `table` _string_ | Table the policy is attached to. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `command` _string_ | Command the policy governs: ALL, SELECT, INSERT, UPDATE or<br />DELETE. |  | MaxLength: 16 <br /> |
| `permissive` _boolean_ | Permissive is true for PERMISSIVE policies (OR-combined, the<br />PostgreSQL default) and false for RESTRICTIVE ones (AND-combined).<br />Recorded explicitly rather than by omission: the two combine in<br />opposite directions, so an evidence document has to state which.<br />Required, unlike SnapshotTable.RLSEnabled. The back-compat hazard<br />that forces that one optional does not exist here: no snapshot<br />captured before model version 1 carries a policy list at all, so<br />there is no stored SnapshotPolicy that could be missing the<br />field. Requiring it stops a hand-authored policy from omitting it<br />and silently meaning RESTRICTIVE. |  |  |
| `roles` _string array_ | Roles the policy applies to. A single "public" entry means all<br />roles. |  | MaxItems: 256 <br /> |
| `using` _string_ | Using is the USING expression — the row filter applied to reads<br />and to the pre-image of writes. |  | MaxLength: 4096 <br /> |
| `withCheck` _string_ | WithCheck is the WITH CHECK expression — the predicate new rows<br />must satisfy. Empty means PostgreSQL falls back to Using. |  | MaxLength: 4096 <br /> |


#### SnapshotSequence



SnapshotSequence is one sequence's shape.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the sequence. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `dataType` _string_ | DataType backing the sequence: smallint, integer or bigint. This<br />is the ceiling on how many values it can ever hand out, so a<br />change to it is a change to the table's capacity. |  | MaxLength: 32 <br /> |
| `incrementBy` _integer_ | IncrementBy is the step. Negative for a descending sequence. |  |  |
| `minValue` _integer_ | MinValue is the lower bound. |  |  |
| `maxValue` _integer_ | MaxValue is the upper bound. |  |  |
| `startValue` _integer_ | StartValue is the value the sequence restarts to. |  |  |


#### SnapshotTable



SnapshotTable captures one table's columns.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the table. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `kind` _string_ | Kind is the PostgreSQL-reported table_type ("BASE TABLE",<br />"VIEW", "FOREIGN", etc.). |  | MaxLength: 32 <br /> |
| `columns` _[SnapshotColumn](#snapshotcolumn) array_ | Columns in ordinal (position) order. |  | MaxItems: 1600 <br /> |
| `viewDefinition` _string_ | ViewDefinition is the defining SELECT, populated only when Kind<br />is VIEW. Empty for base tables.<br />A view's columns are the same before and after its WHERE clause<br />is rewritten, so a snapshot holding only the column list cannot<br />tell a view that filters by tenant from one that no longer does.<br />Only meaningful when the parent's ModelVersion >= 2. |  | MaxLength: 131072 <br /> |
| `rlsEnabled` _boolean_ | RLSEnabled mirrors pg_class.relrowsecurity — whether row-level<br />security is turned on for this table at all.<br />Only meaningful when the parent's ModelVersion >= 1.<br />The json tag carries no omitempty, so the controller always<br />writes it: this is a security fact and an evidence document<br />should assert it rather than imply it by absence.<br />It is nonetheless +optional in the schema. Tables captured before<br />this field existed do not carry it, and this type's whole purpose<br />is that an archived snapshot can be brought back — replayed as<br />restore evidence, or re-applied into a rebuilt cluster. A<br />required field would make every pre-RLS snapshot YAML in the<br />archive fail admission against the new CRD, which is precisely<br />the artifact the registry exists to keep usable. |  |  |
| `rlsForced` _boolean_ | RLSForced mirrors pg_class.relforcerowsecurity — whether RLS<br />also applies to the table's OWNER. This is the bit that usually<br />matters: ENABLE alone exempts the owner, so if the application<br />connects as the role that owns its tables (the common case) then<br />ENABLE-without-FORCE means every policy on the table is bypassed<br />for exactly the connection the policies exist to constrain. On a<br />multi-tenant schema that is a cross-tenant read.<br />A pointer because nil must mean "this snapshot predates the<br />field", not "not forced". Snapshots are immutable, so pre-RLS<br />captures keep a nil here for as long as they are retained;<br />decoding that as false would report FORCE as newly added on<br />every table of every old snapshot compared against a new one.<br />The parent's ModelVersion is the authoritative signal — this<br />nil is the per-table corroboration of it. |  |  |


#### SnapshotTrigger



SnapshotTrigger is one trigger binding.

Trigger names are unique per table, not per schema, so (Table, Name)
is the identity.



_Appears in:_
- [StructuralSnapshot](#structuralsnapshot)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the trigger. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `table` _string_ | Table the trigger is attached to. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `timing` _string_ | Timing is BEFORE, AFTER or INSTEAD OF. |  | MaxLength: 16 <br /> |
| `events` _string array_ | Events the trigger fires on: INSERT, UPDATE, DELETE, TRUNCATE.<br />The inspector builds this list positionally from pg_trigger's<br />tgtype bitfield, so its order is fixed by the query rather than<br />by the catalog and is stable across captures. |  | MaxItems: 4 <br /> |
| `forEachRow` _boolean_ | ForEachRow distinguishes a row trigger from a statement trigger,<br />which decides how many times the function runs.<br />Required, like SnapshotPolicy.Permissive and unlike<br />SnapshotTable.RLSEnabled. The rule is the same in all three<br />places: a field is optional exactly when an archived snapshot<br />could lack it. No snapshot captured below model version 2 carries<br />a trigger list at all, so there is no archive to keep admissible,<br />and requiring it stops a hand-authored trigger that omits the key<br />from silently meaning FOR EACH STATEMENT. |  |  |
| `function` _string_ | Function the trigger calls. Its body is captured separately, in<br />Functions — a rewritten trigger function changes nothing here. |  | MaxLength: 63 <br /> |
| `when` _string_ | When is the optional WHEN clause, extracted from<br />pg_get_triggerdef. Empty when the trigger has none. |  | MaxLength: 4096 <br /> |


#### StageProgress



StageProgress is one entry in MigrationBundleStatus.StageHistory.



_Appears in:_
- [MigrationBundleStatus](#migrationbundlestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name matches RolloutStage.Name. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `startedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | StartedAt is when the controller first dispatched executions<br />for this stage. |  | Required: \{\} <br /> |
| `soakStartedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | SoakStartedAt is when the controller observed all stage targets<br />reach Succeeded and started the soak timer. Empty until that<br />happens. |  |  |
| `completedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#time-v1-meta)_ | CompletedAt is when the controller advanced past this stage<br />(soak elapsed + approval received if required). Empty until<br />completion. |  |  |
| `targets` _integer_ | Targets is the count of DatabaseSchemas selected by this stage. |  |  |
| `succeeded` _integer_ | Succeeded is the count of those that reached Succeeded. When<br />Targets == Succeeded the soak timer starts. |  |  |
| `failed` _integer_ | Failed is the count of executions that ended in Failed. When >0<br />AND the stage's AbortOnFailure is true, the rollout freezes. |  |  |


#### StructuralSnapshot



StructuralSnapshot is the machine-readable shape of a schema at
capture time. Mirrors the drift-inspector output so downstream diff
tools can compare two snapshots without re-querying the database.
Deterministic JSON ordering — tables sorted by name, columns by
ordinal — so two snapshots that observe identical schemas compute
byte-identical serialisations.



_Appears in:_
- [SchemaSnapshotStatus](#schemasnapshotstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `modelVersion` _integer_ | ModelVersion records which model of this struct the capturing<br />controller was built against, so a reader can tell "this schema<br />had no policies" from "this snapshot could not see policies".<br />	0 — implicit. Captured before row-level security was modelled:<br />	    tables, indexes and constraints only.<br />	1 — adds row-level security: per-table RLSEnabled/RLSForced and<br />	    the schema's Policies list.<br />	2 — adds the remaining objects the inspector reads: Enums,<br />	    Extensions, Sequences, Functions, Triggers,<br />	    MaterializedViews, and per-view ViewDefinition.<br />The version is a single counter over the whole struct rather than<br />a flag per dimension, because it answers one question — "was this<br />field populated by a controller that knew about it?" — and every<br />dimension added in the same release shares one answer. A reader<br />compares a dimension only at or above the version that introduced<br />it, and says so out loud below that.<br />Snapshots are immutable once captured and are retained for at<br />least a year — up to a century under a regulatory hold — so the<br />registry holds documents from every model that has ever shipped,<br />permanently. There is no migration to run and no point at which<br />version 0 stops appearing.<br />Deliberately NOT +kubebuilder:default. Structural-schema<br />defaulting is applied when an unset field is read back out of<br />etcd, so a default would relabel every older snapshot in the<br />registry as aware of dimensions nobody captured — inverting the<br />exact property this field exists to provide. |  | Minimum: 0 <br /> |
| `schema` _string_ | Schema is the target schema name. Copied from the parent<br />SchemaSnapshot for self-contained diffing; any mismatch<br />between this and SchemaRef indicates CR corruption. |  | MaxLength: 63 <br />Required: \{\} <br /> |
| `tables` _[SnapshotTable](#snapshottable) array_ | Tables is the ordered list of base tables and views in the<br />schema, excluding Keystone's bookkeeping tables<br />(schema_migrations, keystone_baselines). |  | MaxItems: 2048 <br /> |
| `indexes` _[SnapshotObjectDDL](#snapshotobjectddl) array_ | Indexes is the list of CREATE INDEX definitions harvested from<br />pg_indexes. Type carries btree/hash/gin/etc. |  | MaxItems: 4096 <br /> |
| `constraints` _[SnapshotObjectDDL](#snapshotobjectddl) array_ | Constraints covers primary keys, unique constraints, checks,<br />foreign keys, exclusions — read from pg_constraint. |  | MaxItems: 4096 <br /> |
| `policies` _[SnapshotPolicy](#snapshotpolicy) array_ | Policies is every row-level security policy in the schema, read<br />from pg_policies and sorted by (table, name).<br />Empty means "no policies" only when ModelVersion >= 1; at<br />version 0 it means the capturing controller did not look. |  | MaxItems: 4096 <br /> |
| `enums` _[SnapshotEnum](#snapshotenum) array_ | Enums is every enum type owned by the schema, read from pg_type<br />and pg_enum in sort order.<br />Empty means "no enums" only when ModelVersion >= 2; below that it<br />means the capturing controller did not look. The same holds for<br />every field below. |  | MaxItems: 1024 <br /> |
| `extensions` _[SnapshotExtension](#snapshotextension) array_ | Extensions is every extension installed *into* this schema. An<br />extension the schema merely depends on but does not own — the<br />usual pgcrypto in public — belongs to LogicalDatabase.spec, which<br />is the layer that owns database-scoped objects. |  | MaxItems: 256 <br /> |
| `sequences` _[SnapshotSequence](#snapshotsequence) array_ | Sequences is every sequence in the schema with its bounds and<br />step. The current value is deliberately absent: it changes on<br />every INSERT, and a structural snapshot that moved whenever the<br />data moved would report drift continuously. |  | MaxItems: 2048 <br /> |
| `functions` _[SnapshotFunction](#snapshotfunction) array_ | Functions is every function and procedure in the schema,<br />including bodies. |  | MaxItems: 1024 <br /> |
| `triggers` _[SnapshotTrigger](#snapshottrigger) array_ | Triggers is every user trigger in the schema, sorted by (table,<br />name). Internal triggers — the ones PostgreSQL creates to enforce<br />foreign keys — are excluded by the inspector; they are an<br />implementation detail of the constraints already captured, and<br />recording them would report the same fact twice. |  | MaxItems: 2048 <br /> |
| `materializedViews` _[SnapshotMaterializedView](#snapshotmaterializedview) array_ | MaterializedViews is every materialized view in the schema with<br />its defining SELECT. Refresh state is not recorded, for the same<br />reason sequence values are not. |  | MaxItems: 512 <br /> |


