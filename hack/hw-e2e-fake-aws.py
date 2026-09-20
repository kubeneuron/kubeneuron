#!/usr/bin/env python3
"""A scripted stand-in for the aws CLI, used only by hack/verify-hw-e2e-sweep.sh.

It answers exactly the subset of calls `hack/hw-e2e.sh sweep` makes, from a
JSON state file, and it mutates that state the way AWS would: a stack delete
fails while anything outside the stack still lives in its VPC, a security
group cannot go while an interface or another group's rule references it, an
interface cannot go while attached. The point is not fidelity to AWS in
general; it is that the one sequence which wedged run a17 is reproduced, so
the sweep's cleanup can be proven without a cluster, and its refusals can be
proven against resources that must never be touched.

Every call is appended to FAKE_AWS_LOG. Anything this file does not model
exits 99 with "unsupported" so the verifier fails loudly instead of passing
on a silent no-op.

State file (FAKE_AWS_STATE) shape, tags as plain maps for readability:

  clusters:  [name]
  stacks:    {name: {status, cluster, vpcs: [vpc-id], wedged?: bool}}
  vpcs:      {id: {default: bool, tags}}
  enis:      {id: {vpc, status, attached: bool, requester_managed: bool,
                   interface_type?: str, description, groups: [sg-id], tags}}
             interface_type defaults to "interface" (what the VPC-CNI
             allocates and what the a17 orphan was); "efa", "trunk", ... are
             emitted as given, and an explicit null omits InterfaceType from
             the document altogether.
  sgs:       {id: {vpc, name, tags, referenced_by?: [sg-id]}}
  instances: {id: {state, tags}}
  volumes:   {id: {status, tags}}
"""

import fnmatch
import json
import os
import re
import sys

STATE_PATH = os.environ["FAKE_AWS_STATE"]
LOG_PATH = os.environ["FAKE_AWS_LOG"]


def load():
    with open(STATE_PATH) as f:
        return json.load(f)


def save(state):
    tmp = STATE_PATH + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f, indent=2, sort_keys=True)
    os.replace(tmp, STATE_PATH)


def fail(msg, code=254):
    sys.stderr.write("An error occurred: %s\n" % msg)
    sys.exit(code)


def unsupported(argv):
    sys.stderr.write("fake aws: unsupported call: %s\n" % " ".join(argv))
    sys.exit(99)


def parse(argv):
    """Split argv into (globals, service, operation, options).

    Options are `--name value...`; a value list runs until the next `--flag`.
    """
    glob = {"output": "json", "query": None, "region": None}
    positional = []
    opts = {}
    i = 0
    while i < len(argv):
        a = argv[i]
        if a.startswith("--"):
            key = a[2:]
            vals = []
            i += 1
            while i < len(argv) and not argv[i].startswith("--"):
                vals.append(argv[i])
                i += 1
            if key in glob:
                glob[key] = vals[0] if vals else None
            else:
                opts.setdefault(key, []).extend(vals)
        else:
            positional.append(a)
            i += 1
    if len(positional) < 2:
        unsupported(argv)
    return glob, positional[0], positional[1], opts


def tags_list(tags):
    return [{"Key": k, "Value": v} for k, v in sorted((tags or {}).items())]


def eni_doc(eid, e):
    d = {
        "NetworkInterfaceId": eid,
        "VpcId": e["vpc"],
        "Status": e["status"],
        "RequesterManaged": bool(e.get("requester_managed", False)),
        "Description": e.get("description", ""),
        "Groups": [{"GroupId": g} for g in e.get("groups", [])],
        "TagSet": tags_list(e.get("tags")),
    }
    interface_type = e.get("interface_type", "interface")
    if interface_type is not None:
        d["InterfaceType"] = interface_type
    if e.get("attached"):
        d["Attachment"] = {"AttachmentId": "eni-attach-" + eid, "Status": "attached"}
    return d


def sg_doc(sid, s):
    return {"GroupId": sid, "GroupName": s["name"], "VpcId": s["vpc"], "Tags": tags_list(s.get("tags"))}


def vpc_doc(vid, v):
    return {"VpcId": vid, "IsDefault": bool(v.get("default", False)), "Tags": tags_list(v.get("tags"))}


def instance_doc(iid, inst):
    return {"InstanceId": iid, "State": {"Name": inst["state"]}, "Tags": tags_list(inst.get("tags"))}


def volume_doc(vid, vol):
    return {"VolumeId": vid, "State": vol.get("status", "available"), "Tags": tags_list(vol.get("tags"))}


# Filter attribute -> how to read it off an emitted document.
def attr_values(doc, name):
    if name.startswith("tag:"):
        key = name[4:]
        tags = doc.get("TagSet", doc.get("Tags", []))
        return [t["Value"] for t in tags if t["Key"] == key]
    simple = {
        "vpc-id": "VpcId",
        "status": "Status",
        "description": "Description",
        "interface-type": "InterfaceType",
        "group-name": "GroupName",
        "instance-id": "InstanceId",
        "volume-id": "VolumeId",
    }
    if name in simple:
        v = doc.get(simple[name])
        return [] if v is None else [str(v)]
    if name == "group-id":
        if "Groups" in doc:
            return [g["GroupId"] for g in doc["Groups"]]
        return [doc["GroupId"]] if "GroupId" in doc else []
    if name == "instance-state-name":
        return [doc["State"]["Name"]]
    if name == "is-default":
        return [str(doc["IsDefault"]).lower()]
    unsupported(["filter", name])


def parse_filters(raw):
    out = []
    for f in raw:
        m = re.match(r"^Name=([^,]+),Values=(.*)$", f)
        if not m:
            unsupported(["--filters", f])
        out.append((m.group(1), m.group(2).split(",")))
    return out


def matches(doc, filters):
    for name, values in filters:
        have = attr_values(doc, name)
        if not any(fnmatch.fnmatchcase(h, v) for h in have for v in values):
            return False
    return True


def select(state, kind, ids, filters, missing_error):
    table = state.get(kind, {})
    make = {"enis": eni_doc, "sgs": sg_doc, "vpcs": vpc_doc, "instances": instance_doc, "volumes": volume_doc}[kind]
    if ids:
        for i in ids:
            if i not in table:
                fail("%s: The id '%s' does not exist" % (missing_error, i))
        chosen = ids
    else:
        chosen = sorted(table)
    docs = [make(i, table[i]) for i in chosen]
    return [d for d in docs if matches(d, filters)]


def emit(glob, envelope_key, docs, nested=None):
    """Print either the JSON envelope or the text projection of --query."""
    if glob["output"] == "json":
        if nested:
            print(json.dumps({envelope_key: [{nested: docs}]} if docs else {envelope_key: []}))
        else:
            print(json.dumps({envelope_key: docs}))
        return
    query = glob["query"] or ""
    head = query.split("|")[0]
    fields = re.findall(r"\.([A-Za-z]+)", head)
    if not fields:
        unsupported(["--query", query])
    field = fields[-1]
    if "[0]" in head:
        print(str(docs[0][field]) if docs else "None")
        return
    values = [str(d[field]) for d in docs]
    if "join(" in query:
        print(",".join(values))
    elif values:
        print("\t".join(values))


def stack_owned_sgs(state, stack, vpc):
    return [
        sid
        for sid, s in state["sgs"].items()
        if s["vpc"] == vpc and s.get("tags", {}).get("alpha.eksctl.io/cluster-name") == stack["cluster"]
    ]


def sg_blockers(state, sid):
    """Why a security group cannot be deleted right now, or None."""
    s = state["sgs"][sid]
    if s["name"] == "default":
        return "CannotDelete: the default group cannot be deleted"
    refs = [eid for eid, e in state["enis"].items() if sid in e.get("groups", [])]
    if refs:
        return "DependencyViolation: resource %s has a dependent object (%s)" % (sid, ",".join(refs))
    rules = [r for r in s.get("referenced_by", []) if r in state["sgs"]]
    if rules:
        return "DependencyViolation: resource %s is referenced by rules in %s" % (sid, ",".join(rules))
    return None


def wait_stack_delete(state, name):
    """Resolve a pending stack delete the way CloudFormation does.

    A stack that is not DELETE_IN_PROGRESS has no delete to resolve: `wait`
    on it returns immediately with the stack unchanged, so a sweep that only
    waits (it saw DELETE_IN_PROGRESS and chose not to race) is modelled
    faithfully. A stack in DELETE_IN_PROGRESS (set by delete-stack, or by the
    fixture to stand for an eksctl delete still running) either finishes or
    lands in DELETE_FAILED with its surviving resources intact.
    """
    stack = state["stacks"].get(name)
    if stack is None:
        return 0
    if stack["status"] != "DELETE_IN_PROGRESS":
        return 0 if stack["status"] == "DELETE_COMPLETE" else 255
    if stack.get("wedged") or stack["cluster"] in state["clusters"]:
        stack["status"] = "DELETE_FAILED"
        return 255
    for vpc in stack["vpcs"]:
        # The stack removes its own groups first, and a group an interface
        # still holds cannot go — this is the a17 shape: the VPC-CNI
        # interface carried BOTH the EKS cluster group and the stack's shared
        # node group, so the shared group (whose rule references the cluster
        # group) survived the first delete and pinned the cluster group until
        # the stack was retried once with the interface gone. Any group that
        # can go, goes: CloudFormation keeps what it managed to delete.
        for sid in stack_owned_sgs(state, stack, vpc):
            if sg_blockers(state, sid):
                stack["status"] = "DELETE_FAILED"
                return 255
            del state["sgs"][sid]
        leftovers = [eid for eid, e in state["enis"].items() if e["vpc"] == vpc]
        leftovers += [sid for sid, s in state["sgs"].items() if s["vpc"] == vpc and s["name"] != "default"]
        if leftovers:
            stack["status"] = "DELETE_FAILED"
            return 255
        for sid in [sid for sid, s in state["sgs"].items() if s["vpc"] == vpc]:
            del state["sgs"][sid]
        state["vpcs"].pop(vpc, None)
    del state["stacks"][name]
    return 0


def main(argv):
    with open(LOG_PATH, "a") as log:
        log.write("aws " + " ".join(argv) + "\n")
    glob, service, op, opts = parse(argv)
    state = load()
    query = glob["query"] or ""

    if service == "eks" and op == "describe-cluster":
        name = opts.get("name", [""])[0]
        if name not in state["clusters"]:
            fail("ResourceNotFoundException: No cluster found for name: %s" % name)
        if glob["output"] == "json":
            print(json.dumps({"cluster": {"name": name}}))
        else:
            print(name)
        return 0

    if service == "eks" and op == "list-clusters":
        names = sorted(state["clusters"])
        m = re.search(r"@=='([^']*)'", query)
        if m:
            names = [n for n in names if n == m.group(1)]
        m = re.search(r"starts_with\(@,\s*'([^']*)'\)", query)
        if m:
            names = [n for n in names if n.startswith(m.group(1))]
        if glob["output"] == "json":
            print(json.dumps(names))
        else:
            print(",".join(names) if "join(" in query else "\t".join(names))
        return 0

    if service == "ec2" and op in ("describe-network-interfaces", "describe-security-groups", "describe-vpcs", "describe-instances", "describe-volumes"):
        kind, envelope, ids_opt, err = {
            "describe-network-interfaces": ("enis", "NetworkInterfaces", "network-interface-ids", "InvalidNetworkInterfaceID.NotFound"),
            "describe-security-groups": ("sgs", "SecurityGroups", "group-ids", "InvalidGroup.NotFound"),
            "describe-vpcs": ("vpcs", "Vpcs", "vpc-ids", "InvalidVpcID.NotFound"),
            "describe-instances": ("instances", "Reservations", "instance-ids", "InvalidInstanceID.NotFound"),
            "describe-volumes": ("volumes", "Volumes", "volume-ids", "InvalidVolume.NotFound"),
        }[op]
        docs = select(state, kind, opts.get(ids_opt, []), parse_filters(opts.get("filters", [])), err)
        emit(glob, envelope, docs, nested="Instances" if kind == "instances" else None)
        return 0

    if service == "ec2" and op == "delete-network-interface":
        eid = opts["network-interface-id"][0]
        e = state["enis"].get(eid)
        if e is None:
            fail("InvalidNetworkInterfaceID.NotFound: %s" % eid)
        if e["status"] != "available" or e.get("attached"):
            fail("InvalidParameterValue: Network interface '%s' is currently in use" % eid)
        del state["enis"][eid]
        save(state)
        return 0

    if service == "ec2" and op == "delete-security-group":
        sid = opts["group-id"][0]
        if sid not in state["sgs"]:
            fail("InvalidGroup.NotFound: %s" % sid)
        why = sg_blockers(state, sid)
        if why:
            fail(why)
        del state["sgs"][sid]
        save(state)
        return 0

    if service == "ec2" and op == "terminate-instances":
        for iid in opts.get("instance-ids", []):
            if iid in state["instances"]:
                state["instances"][iid]["state"] = "terminated"
        save(state)
        print("{}")
        return 0

    if service == "ec2" and op == "wait":
        # instance-terminated: terminate-instances above is synchronous here.
        return 0

    if service == "ec2" and op == "delete-volume":
        state["volumes"].pop(opts["volume-id"][0], None)
        save(state)
        return 0

    if service == "cloudformation" and op == "list-stacks":
        m = re.search(r"starts_with\(StackName,\s*'([^']*)'\)", query)
        prefix = m.group(1) if m else ""
        statuses = opts.get("stack-status-filter")
        names = []
        for name, s in sorted(state["stacks"].items()):
            if not name.startswith(prefix):
                continue
            if statuses is not None and s["status"] not in statuses:
                continue
            if statuses is None and s["status"] == "DELETE_COMPLETE":
                continue
            names.append(name)
        print(",".join(names) if "join(" in query else "\t".join(names))
        return 0

    if service == "cloudformation" and op == "describe-stacks":
        name = opts["stack-name"][0]
        s = state["stacks"].get(name)
        if s is None:
            fail("ValidationError: Stack with id %s does not exist" % name)
        if glob["output"] == "json":
            print(json.dumps({"Stacks": [{"StackName": name, "StackStatus": s["status"]}]}))
        else:
            print(s["status"])
        return 0

    if service == "cloudformation" and op == "describe-stack-resources":
        name = opts["stack-name"][0]
        s = state["stacks"].get(name)
        if s is None:
            fail("ValidationError: Stack with id %s does not exist" % name)
        if opts.get("logical-resource-id", [""])[0] != "VPC":
            unsupported(argv)
        vpcs = s.get("vpcs", [])
        if glob["output"] == "json":
            print(json.dumps({"StackResources": [{"LogicalResourceId": "VPC", "ResourceType": "AWS::EC2::VPC", "PhysicalResourceId": v} for v in vpcs]}))
        elif vpcs:
            print("\t".join(vpcs))
        return 0

    if service == "cloudformation" and op == "delete-stack":
        name = opts["stack-name"][0]
        if name in state["stacks"]:
            state["stacks"][name]["status"] = "DELETE_IN_PROGRESS"
            save(state)
        return 0

    if service == "cloudformation" and op == "wait":
        rc = wait_stack_delete(state, opts["stack-name"][0])
        save(state)
        return rc

    if service == "iam" and op == "get-role":
        fail("NoSuchEntity: The role with name %s cannot be found." % opts.get("role-name", [""])[0])

    if service == "ecr" and op == "batch-delete-image":
        print("{}")
        return 0

    unsupported(argv)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
