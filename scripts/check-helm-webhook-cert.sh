#!/usr/bin/env bash
# check-helm-webhook-cert.sh — guard the self-signed webhook cert's identity
# against the capped Service name (#1885).
#
# WHY THIS EXISTS
#
# The API server validates the serving cert against the Service DNS name, so a
# Service name that is capped while the cert's CN or SANs are not breaks every
# admission call. The chart's own helm-unittest suite cannot assert that: the
# SANs and the CN live only inside the DER of the base64 PEM in the Secret's
# data.tls.crt / data.ca.crt, so the suite covers only the cert-manager path's
# spec.dnsNames. This renders the chart and decodes both certs with openssl,
# which is what a reviewer otherwise has to do by hand.
#
# The fixture is the same boilerplate name the unit suite uses, long enough
# that the metrics Service, the webhook Service and the CA CN all hit the cap.
# The expected strings are hardcoded on purpose: recomputing them from the same
# truncation arithmetic would re-implement the logic under test and pass when
# the logic is wrong.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

command -v helm >/dev/null 2>&1 || { echo "❌ helm is required"; exit 2; }
command -v openssl >/dev/null 2>&1 || { echo "❌ openssl is required"; exit 2; }
python3 -c 'import yaml' >/dev/null 2>&1 || { echo "❌ python3 with PyYAML is required (pip install pyyaml)"; exit 2; }

FIXTURE="llmkube-example-release-region-primary-cluster-production"

# A temp FILE, not an env var: the render carries base64 cert blobs and can
# exceed the OS env+argv limit (E2BIG).
MANIFEST="$(mktemp)"
trap 'rm -f "$MANIFEST"' EXIT

helm template llmkube "$REPO_ROOT/charts/llmkube" \
  --namespace llmkube-system \
  --set "fullnameOverride=$FIXTURE" > "$MANIFEST"

MANIFEST="$MANIFEST" python3 - <<'PY'
import base64, os, subprocess, sys, yaml

with open(os.environ["MANIFEST"]) as fh:
    docs = [d for d in yaml.safe_load_all(fh) if d]

secret = next((d for d in docs if d.get("kind") == "Secret"), None)
if secret is None:
    print("❌ no Secret rendered; this guard needs webhook.enabled=true")
    sys.exit(1)

EXPECTED_SERVING_CN = "llmkube-example-release-region-primary-cluster-producti-webhook"
EXPECTED_SANS = {
    EXPECTED_SERVING_CN + ".llmkube-system.svc",
    EXPECTED_SERVING_CN + ".llmkube-system.svc.cluster.local",
}
EXPECTED_CA_CN = "llmkube-example-release-region-primary-cluster-produ-webhook-ca"
CA_CN_MAX = 64  # RFC 5280 ub-common-name


def openssl(pem_text, *args):
    r = subprocess.run(["openssl", "x509", "-noout", *args],
                       input=pem_text, capture_output=True, text=True)
    if r.returncode != 0:
        print("❌ openssl x509 failed: %s" % r.stderr.strip())
        sys.exit(1)
    return r.stdout


def pem(key):
    return base64.b64decode(secret["data"][key]).decode()


def common_name(pem_text):
    # RFC2253 prints the subject as subject=CN=<cn>; genCA sets only a CN.
    out = openssl(pem_text, "-subject", "-nameopt", "RFC2253").strip()
    return out.split("CN=", 1)[1]


def sans(pem_text):
    out = openssl(pem_text, "-ext", "subjectAltName")
    out = out.replace("X509v3 Subject Alternative Name:", " ")
    return {p.strip()[len("DNS:"):] for p in out.split(",")
            if p.strip().startswith("DNS:")}


failed = False

serving_cn = common_name(pem("tls.crt"))
if serving_cn != EXPECTED_SERVING_CN:
    failed = True
    print("❌ serving cert CN is %r (len %d), expected %r (len %d)"
          % (serving_cn, len(serving_cn),
             EXPECTED_SERVING_CN, len(EXPECTED_SERVING_CN)))

serving_sans = sans(pem("tls.crt"))
if serving_sans != EXPECTED_SANS:
    failed = True
    print("❌ serving cert SANs are %s, expected %s"
          % (sorted(serving_sans), sorted(EXPECTED_SANS)))

ca_cn = common_name(pem("ca.crt"))
if ca_cn != EXPECTED_CA_CN:
    failed = True
    print("❌ CA cert CN is %r (len %d), expected %r (len %d)"
          % (ca_cn, len(ca_cn), EXPECTED_CA_CN, len(EXPECTED_CA_CN)))
if len(ca_cn) > CA_CN_MAX:
    failed = True
    print("❌ CA cert CN is %d chars, over RFC 5280's ub-common-name cap of %d"
          % (len(ca_cn), CA_CN_MAX))

if failed:
    print("   The serving cert has to name the capped Service exactly, or every")
    print("   admission call fails. Fix the name composition, not this guard.")
    sys.exit(1)

print("✅ Webhook cert names the capped Service (%s); CA CN fits RFC 5280."
      % EXPECTED_SERVING_CN)
PY
