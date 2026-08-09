# OpsCart Kubernetes Failure Lab

This directory contains a reproducible Kubernetes environment for evaluating
OpsCart classification, prioritization, grouping, operational history, and
read-only investigation guidance.

The lab deploys both intentionally unhealthy and healthy workloads so OpsCart
must distinguish actionable failures from normal cluster state.

> **Do not deploy this lab to a production cluster.** It intentionally creates
> failing workloads, invalid image pulls, a privileged container, missing
> NetworkPolicy coverage, and unused PersistentVolumeClaims.

## Included scenarios

| Scenario                     |                            Coverage |
| ---------------------------- | ----------------------------------: |
| CrashLoopBackOff             |                       4 Deployments |
| ImagePullBackOff             |                       3 Deployments |
| OOMKilled                    |                        1 Deployment |
| Liveness probe failure       |                        1 Deployment |
| Privileged container         |                        1 Deployment |
| Missing NetworkPolicy        |       2 intended namespace findings |
| Zero-replica workload        |                       2 Deployments |
| Unused PersistentVolumeClaim |                              3 PVCs |
| Healthy comparison workloads | Multiple Deployments across the lab |

The workloads are distributed across eight application-style namespaces:

* `payments`
* `inventory`
* `auth`
* `notifications`
* `data-pipeline`
* `api-gateway`
* `platform-ops`
* `reporting`

Every lab namespace is labeled:

```text
opscart.io/failure-lab=true
```

The cleanup script uses this label to avoid deleting unrelated namespaces.

## Directory structure

```text
failure-lab/
├── README.md
├── manifests/
│   ├── scenarios.yaml
│   └── probe-failure.yaml
└── scripts/
    ├── setup.sh
    └── cleanup.sh
```

## Requirements

* A disposable or non-production Kubernetes cluster
* `kubectl` configured with the intended cluster context
* Permission to create and delete namespaces and namespaced resources
* OpsCart CLI or dashboard

The privileged-container scenario may be rejected by clusters enforcing
restricted Pod Security Admission. Other scenarios can still be created.

## Confirm the target cluster

Before setup, verify the current context:

```bash
kubectl config current-context
kubectl cluster-info
```

Make sure this is the cluster where you intend to create the simulation.

## Deploy the lab

From the repository root:

```bash
./examples/failure-lab/scripts/setup.sh
```

For non-interactive execution:

```bash
./examples/failure-lab/scripts/setup.sh --yes
```

The setup script refuses to modify any expected namespace that already exists
without the `opscart.io/failure-lab=true` ownership label.

Allow approximately 30–60 seconds for restart loops, image-pull errors, probe
events, and OOM termination evidence to develop.

## Inspect the lab

List its namespaces:

```bash
kubectl get namespaces \
  -l opscart.io/failure-lab=true
```

Inspect pods across the lab namespaces:

```bash
for namespace in \
  payments inventory auth notifications \
  data-pipeline api-gateway platform-ops reporting
do
  kubectl get pods -n "$namespace"
done
```

Inspect recent warning events:

```bash
kubectl get events -A \
  --field-selector type=Warning \
  --sort-by='.lastTimestamp'
```

Verify the probe-failure scenario:

```bash
kubectl get pods \
  -n payments \
  -l app=checkout-api

kubectl get events \
  -n payments \
  --field-selector involvedObject.kind=Pod \
  --sort-by='.lastTimestamp' |
grep -i 'probe'
```

## Run OpsCart CLI triage

```bash
opscart-scan triage \
  --cluster "$(kubectl config current-context)" \
  --next-steps
```

To focus on the namespace containing the probe failure:

```bash
opscart-scan triage \
  --cluster "$(kubectl config current-context)" \
  -n payments \
  --next-steps
```

The first scan establishes the initial operational-memory observation. Run
additional scans to observe continuing conditions:

```bash
opscart-scan triage \
  --cluster "$(kubectl config current-context)"
```

## Evaluate the output

When reviewing OpsCart, consider:

1. Are unrelated failures kept separate?
2. Are related symptoms grouped appropriately?
3. Does the ordering reflect severity, persistence, and affected resources?
4. Is the reported evidence consistent with Kubernetes events and status?
5. Are the suggested inspection commands relevant and read-only?
6. Do repeated scans represent continuing conditions correctly?

The lab creates reproducible evidence, but it does not prescribe one universally
correct priority order for every operational environment.

## Cleanup

From the repository root:

```bash
./examples/failure-lab/scripts/cleanup.sh
```

For non-interactive cleanup:

```bash
./examples/failure-lab/scripts/cleanup.sh --yes
```

The cleanup script deletes namespaces labeled:

```text
opscart.io/failure-lab=true
```

Before confirming cleanup, it displays the namespaces that will be removed.

Verify cleanup:

```bash
kubectl get namespaces \
  -l opscart.io/failure-lab=true
```

No lab namespaces should remain.

## Reapply the lab

After cleanup, the complete environment can be recreated:

```bash
./examples/failure-lab/scripts/setup.sh
```

A fresh deployment receives new Kubernetes object identities. If OpsCart retains
history from a previous deployment under the same cluster identity, review that
history carefully when evaluating recovery or reopening behavior.
