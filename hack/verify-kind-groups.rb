# SPDX-License-Identifier: Apache-2.0

require "yaml"

jobs = YAML.safe_load(File.read(ARGV.shift)).fetch("jobs")
expected = ARGV.sort
installer = jobs.fetch("kind-other").fetch("strategy").fetch("matrix").fetch("include")
  .select { |row| row.fetch("target", "test-integration-kind") == "test-integration-kind" }
  .map { |row| row.fetch("suite") }

%w[amd64 arm64].each do |arch|
  groups = jobs.fetch("kind-#{arch}-groups").fetch("strategy").fetch("matrix").fetch("suite")
  actual = (groups + installer).sort
  abort "#{arch}: expected #{expected.inspect}, got #{actual.inspect}" unless actual == expected
end
