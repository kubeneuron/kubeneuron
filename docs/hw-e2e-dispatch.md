# Dispatching the hardware E2E from GitHub Actions

The `hw-e2e.yaml` workflow has run green driven locally (`hack/hw-e2e.sh`,
2026-08-05, cluster `kubeneuron-e2e10`), but has never been dispatched from
GitHub Actions itself: that needs an AWS IAM OIDC role, which is deliberately
NOT created by any script in this repo — it is durable account infrastructure
a human sets up once. This page is the exact recipe.

## 1. OIDC provider (once per account)

```sh
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com \
  --client-id-list sts.amazonaws.com
```

## 2. Trust policy — environment-scoped, not branch-scoped

The workflow declares `environment: gpu-lab`, so the token's `sub` is
`repo:<org>/<repo>:environment:gpu-lab`. Scope trust to THAT — a
branch-scoped subject would let any branch dispatch the lab.

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": { "Federated": "arn:aws:iam::<ACCOUNT>:oidc-provider/token.actions.githubusercontent.com" },
    "Action": "sts:AssumeRoleWithWebIdentity",
    "Condition": {
      "StringEquals": {
        "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
        "token.actions.githubusercontent.com:sub": "repo:<ORG>/<REPO>:environment:gpu-lab"
      }
    }
  }]
}
```

## 3. Permission policy — bounded to the e2e naming scheme

eksctl needs CloudFormation, EC2, and IAM role creation; bound every one:

- `aws:RequestedRegion` condition = `us-east-1` on everything.
- CloudFormation resources: `arn:aws:cloudformation:*:*:stack/eksctl-kubeneuron-e2e*`.
- EKS cluster names: `kubeneuron-e2e*`.
- `ec2:RunInstances` with an instance-type condition allowing only
  `g4dn.*` and `m6i.large` (the CPU nodegroup).
- `iam:CreateRole` only with a mandatory `iam:PermissionsBoundary` — attach
  a boundary policy that denies `iam:*` (except read), `organizations:*`,
  and `account:*`, so no role eksctl creates can mint further IAM power.
- `iam:CreateRole`/`DeleteRole`/`PutRolePolicy` resources:
  `arn:aws:iam::*:role/eksctl-kubeneuron-e2e*` and
  `arn:aws:iam::*:role/kubeneuron-e2e*` (the run-scoped recycle role).

## 4. Wire it

Set the role ARN as the `gpu-lab` environment variable/secret the workflow's
`aws-actions/configure-aws-credentials` step consumes, keep the typed
confirmation input, and require environment reviewers on `gpu-lab`. The first
dispatched run proves reproducibility without a workstation; until then the
weekly cron should stay disabled or it will fail red on missing credentials.
