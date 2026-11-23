# SPDX-License-Identifier: Apache-2.0
# A fault reported while the controller still runs the previous revision of
# the published configuration. $before is the digest it had loaded, $after the
# digest of the next revision, which no longer lets the policy authorise drain.
# $revision says how: "profile" names a missing profile under Automatic,
# "approval" moves the policy to Approval with a qualified profile.
{
  profile: "^automatic policy [^ ]+ did not authorise this step: automatic replacement is not allowed on this node — node profile kind-nvidia is not listed in the policy's spec\\.automatic\\.qualifiedProfiles ",
  approval: "^step requires human approval$"
}[$revision] as $park
| (.audit | sort_by(.id)) as $audit
| [$audit[] | select(.action == "recovery-held")] as $holds
| [$audit[] | select(.result // "" | startswith("step-start: "))] as $starts
| [$audit[] | select(.to_state == "AWAITING_APPROVAL")] as $parks
| ($holds | length) > 0
# Only the first step is held, and every hold names the two revisions.
and all($holds[]; (.result | test("^cordon held( by policy [^:]+)?: the controller has not yet loaded the published configuration;"))
  and .params.loaded_config_digest == $before and .params.published_config_digest == $after)
# Nothing starts before the last hold: until the reload, no step runs under
# the previous revision. Afterwards only the system cordon may start.
and all($starts[]; .id > $holds[-1].id and .action == "cordon" and .actor == "system")
# Under the new revision drain waits for a person.
and ($parks | length) > 0
and ($parks[0] | .action == "drain" and .from_state != "AWAITING_APPROVAL" and .id > $holds[-1].id
  and (.result // "" | test($park)))
and all($audit[]; .action != "policy-authorised" and .action != "approval-approved")
