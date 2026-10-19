#!/usr/bin/env python3
"""Turn the checked-in VMAgent custom resource into a plain vmagent
-promscrape.config file for the kind GPU observability fixture.

Reads deploy/kubernetes/dependencies/observability/vmagent.yaml (path given
as argv[1]) and writes a Prometheus-format config to stdout whose
scrape_configs are spec.inlineScrapeConfig parsed verbatim and whose global
scrape_interval is spec.scrapeInterval. spec.externalLabels, when the
resource carries any, become global.external_labels; the shipped resource
carries none, which is what keeps `cluster` absent.

No job, relabel rule or label is added or removed: the point of the fixture
is to run the shipped contract.
"""
import sys

import yaml

cr_path = sys.argv[1]
with open(cr_path, encoding="utf-8") as fh:
    cr = yaml.safe_load(fh)

if cr.get("kind") != "VMAgent":
    sys.exit(f"{cr_path}: kind {cr.get('kind')!r}, want VMAgent")
spec = cr["spec"]
scrape_configs = yaml.safe_load(spec["inlineScrapeConfig"])
if not isinstance(scrape_configs, list) or not scrape_configs:
    sys.exit(f"{cr_path}: spec.inlineScrapeConfig is not a non-empty list")
if not any(job.get("job_name") == "dcgm-exporter" for job in scrape_configs):
    sys.exit(f"{cr_path}: no dcgm-exporter job in spec.inlineScrapeConfig")

global_section = {"scrape_interval": spec["scrapeInterval"]}
if spec.get("externalLabels"):
    global_section["external_labels"] = spec["externalLabels"]

yaml.safe_dump(
    {"global": global_section, "scrape_configs": scrape_configs},
    sys.stdout,
    sort_keys=False,
    default_flow_style=False,
)
