# AWS Region Account Nuke

A deliberately small CLI for removing selected high-cost AWS resources from one account and region.
It supports MSK, ElastiCache, OpenSearch, Aurora, standalone RDS instances, and EFS.

The command is safe by default: it only produces a plan unless `--execute` is supplied, requires an
explicit account and service list, and requires at least one resource ID or tag selector.

## Build

```sh
go build ./cmd/aws-region-account-nuke
```

AWS credentials use the normal SDK credential chain. `--profile` can select an AWS shared-config
profile; SSO, environment credentials, and assumed-role profiles work through that chain.

## Preview a plan

```sh
./aws-region-account-nuke \
  --account 123456789012 \
  --region ap-south-1 \
  --services msk,elasticache,opensearch,rds \
  --tag Environment=preview
```

Selectors are combined with OR: a resource is selected if its ID/name/ARN matches any `--resource-id`,
or if all supplied `--tag` values match. Repeat either flag as needed. Supported service names are:

- `msk`: provisioned and serverless MSK clusters
- `elasticache`: replication groups, standalone cache clusters, and serverless caches
- `opensearch`: managed domains and serverless collections
- `rds`: Aurora clusters and standalone RDS instances
- `efs`: standalone file systems and same-account, same-region replication pairs

Resources tagged `DoNotDelete=true` are always shown as protected and never deleted.
Deleting an Aurora cluster also deletes its member DB instances, because AWS requires this ordering.
Every member is checked for the protection tag and deletion protection before any member is changed.

## EFS cleanup

```sh
./aws-region-account-nuke --account 123456789012 --region ap-south-1 \
  --services efs --resource-id fs-11111111 --resource-id fs-22222222
```

For standalone EFS, select just the file system. For replication, select **both source and destination**
(by ID, name, ARN, or matching tags). Removing replication alone leaves the destination file system
and its storage charges behind, so the CLI requires both ends to be selected and unprotected.
It checks this during planning and again before deletion. It does not implicitly expand selectors.

Cleanup removes replication first and waits for both ends to detach, removes access points and mount
targets, waits for their deletion, and finally deletes each selected file system and its data.
EFS deletions are serialized to avoid competing replication-removal requests. Prerequisite waits
still run with `--wait=false`; only the final file-system wait is skipped. Protected access points
also prevent cleanup of their file system. Existing AWS Backup recovery points, security groups,
subnets, and KMS keys are retained; mount-target network interfaces are removed by EFS.

**Cross-region and cross-account replication are refused**, because removing their configuration
would affect resources outside the confirmed account/region. Detach such replication separately,
then run the CLI against each account/region explicitly. There is no local-only replication cleanup
fallback that could leave an unrecoverable remote configuration.

The EFS IAM permissions are `elasticfilesystem:DescribeFileSystems`,
`elasticfilesystem:DescribeReplicationConfigurations`, `elasticfilesystem:DescribeAccessPoints`,
`elasticfilesystem:DescribeMountTargets`, `elasticfilesystem:DeleteReplicationConfiguration`,
`elasticfilesystem:DeleteAccessPoint`, `elasticfilesystem:DeleteMountTarget`, and
`elasticfilesystem:DeleteFileSystem`. Dry runs need only the describe permissions.

## Execute

Review the plan, then add `--execute`. The command asks you to type the 12-digit account ID:

```sh
./aws-region-account-nuke ... --execute
```

For unattended cleanup, pass the same value with `--confirm-account 123456789012`. This is deliberately
not a generic `--yes` switch. Use `--deny-account` repeatedly, or set a comma-separated
`AWS_NUKE_DENY_ACCOUNTS`, to block production accounts. A denied account cannot be overridden.

Deletion protection is not changed by default. `--disable-deletion-protection` permits the CLI to turn
it off for selected RDS/Aurora resources before deletion. RDS/Aurora final snapshots are skipped, while
manual snapshots, automated backups, subnet groups, parameter groups, and security groups are retained.

By default, execution waits for each resource to disappear and deletes at most two resources at once.
Use `--wait=false`, `--parallel`, and `--timeout` to adjust this. A timestamped JSON report is written for
both dry runs and executions; use `--report` to choose its path.

## Safety contract

1. STS must report exactly the account supplied with `--account`.
2. The account must not appear in the denylist.
3. Services and selectors must be explicit; there is no implicit "all resources" mode.
4. `DoNotDelete=true` cannot be overridden.
5. Dry-run is the default, and execution requires account-ID confirmation.
6. The CLI does not delete snapshots or shared networking/configuration resources.

The IAM role running the command needs read/tag-list permissions for selected services and STS, plus
the corresponding delete permissions. It only needs modify permissions when
`--disable-deletion-protection` is used.
