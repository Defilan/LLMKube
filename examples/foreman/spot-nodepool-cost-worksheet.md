# Spot GPU NodePool Cost Worksheet

This worksheet estimates the cost of running Foreman verification-gate
Jobs on spot GPU capacity via the Karpenter NodePool defined in
`spot-nodepool-karpenter.yaml`.

## Assumptions

- Region: us-east-1
- Instance: g5.xlarge (1x NVIDIA A10G, 16 GiB VRAM)
- On-demand price: ~$0.526/hr
- Spot discount: ~60-70% off on-demand
- Spot price: ~$0.16-0.21/hr
- Gate Job runtime: 5-15 minutes (fmt, vet, lint, test)
- Karpenter consolidationPolicy: WhenEmpty (nodes terminate within
  1 minute of going idle)

## Per-Run Cost

| Item | Value |
|------|-------|
| Spot price (g5.xlarge) | $0.18/hr (avg) |
| Job runtime | 10 min (avg) |
| Provisioning overhead | 2 min |
| Total time | 12 min |
| Cost per gate run | $0.036 |

## Daily Cost (10 gate runs/day)

| Item | Value |
|------|-------|
| Runs per day | 10 |
| Cost per run | $0.036 |
| Daily cost | $0.36 |
| Monthly cost (30 days) | $10.80 |

## Comparison: On-Demand vs Spot

| Scenario | On-demand | Spot | Savings |
|----------|-----------|------|---------|
| 10 runs/day | $1.75/day | $0.36/day | 79% |
| 50 runs/day | $8.75/day | $1.80/day | 79% |
| 100 runs/day | $17.50/day | $3.60/day | 79% |

## Notes

- Spot interruptions are handled by the Job's `backoffLimit: 3` and
  `restartPolicy: OnFailure`. If a spot instance is reclaimed, the Job
  reschedules on a new node.
- The `consolidationPolicy: WhenEmpty` ensures Karpenter terminates
  idle nodes within 1 minute, minimizing idle cost.
- For higher throughput, add more instance types to the NodePool
  requirements (e.g., g5.2xlarge) to increase spot capacity availability.
- Actual spot prices vary by region and time. Use AWS Spot Instance
  Advisor or `aws ec2 describe-spot-price-history` for current pricing.
