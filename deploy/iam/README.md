# Cloud permissions

Jin needs cloud permissions only for **direct** upgrades and the EKS support calendar. GitOps mode
changes nothing in the cloud: your pipeline applies the merged pull requests.

## Amazon EKS

- [`jin-eks-direct-upgrade.json`](jin-eks-direct-upgrade.json): control-plane, add-on and node-group
  upgrades for one cluster. Replace `REGION`, `ACCOUNT_ID` and `CLUSTER_NAME`.
- The support calendar (plans) additionally needs `eks:DescribeClusterVersions` on `*`.

Jin uses the same profile, region and role as the kubeconfig's `aws eks get-token` exec plugin.
To **add clusters from the UI**, the identity also needs `eks:ListClusters` (`*`) and `eks:DescribeCluster`,
plus an EKS access entry on each cluster (the UI prints the exact commands if it is missing).

## Google GKE

Grant the identity Jin runs as (Application Default Credentials) a custom role on the project with:

```
container.clusters.get
container.clusters.update
container.operations.get
container.operations.list
```

`roles/container.clusterAdmin` also works but grants more than Jin needs.

## Azure AKS

Jin uses `DefaultAzureCredential` (environment, workload identity, managed identity or Azure CLI).
Assign a custom role scoped to the cluster resource with:

```json
{
  "Actions": [
    "Microsoft.ContainerService/managedClusters/read",
    "Microsoft.ContainerService/managedClusters/write",
    "Microsoft.ContainerService/managedClusters/agentPools/read",
    "Microsoft.ContainerService/managedClusters/agentPools/write",
    "Microsoft.ContainerService/managedClusters/upgradeProfiles/read"
  ]
}
```

AKS clusters also need their subscription, resource group and name in the Jin cluster settings.

## GitHub (GitOps mode)

Use a fine-grained token (or a GitHub App installation token) limited to the infrastructure
repository with **Contents: read and write** and **Pull requests: read and write**. Add it under
**Integrations** in the UI (stored encrypted), or put it in `GITHUB_TOKEN` (or another `GITHUB_*` /
`JIN_GITHUB_*` variable) in the Jin server's environment.
