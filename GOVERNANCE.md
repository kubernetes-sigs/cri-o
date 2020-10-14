# CRI-O contributor tiers

* CRI-O has three tiers of code contributors: maintainers, emeritus maintainers, and community members.
* Anyone can (and is encouraged to) be a community member, whose responsibilities are up to the contributor (submitting PRs, reviewing PRs).
* Maintainers are the set of people that have final say over the direction of the project.

# Process for becoming a maintainer

* Express an interest to the current maintainers that you wish to become one.
* Becoming a maintainer indicates you are going to be spending substantial (>25%) development time on CRI-O for the foreseeable future.
* You should have domain expertise and be proficient in Go.
* We will expect you to start contributing PRs under the guidance of current maintainers.
* To start, we may ask you to take some issues from our backlog.
* As you gain experience with the code base and our standards, we will ask you to do code reviews for incoming PRs (i.e., all maintainers are expected to shoulder a proportional share of community reviews).
* After a period of approximately 2-3 months of working together and evaluating your contributions to the project, the existing maintainers will confer and decide whether to grant maintainer status or not. We make no guarantees on the length of time this will take, but 2-3 months is the approximate goal.

## Maintainer responsibilities

* Monitor our channel (#crio) on the Kubernetes Slack instance and our IRC channel (#cri-o) on Freenode (delayed response is perfectly acceptable)
* Triage GitHub issues and perform pull request reviews for other maintainers and the community. The areas of specialization listed in [OWNERS.md](OWNERS.md) can be used to help with routing an issue/question to the right person.
* Triage CI issues - file issues for known flaky CI runs or bugs, and either fix or find someone to fix any breakages.
* During GitHub issue triage, apply all applicable [labels](https://github.com/cri-o/cri-o/labels) to each new issue. Labels are extremely useful for future issue follow up. Which labels to apply is somewhat subjective so just use your best judgment. A few of the most important labels that are not self explanatory are:
  * **good first issue**: Mark any issue that can reasonably be accomplished by a new contributor with this label.
  * **help wanted**: Unless it is immediately obvious that someone is going to work on an issue (and if so assign it), mark it help wanted.
  * **CRI change**: If sufficiently fixing the issue involves a change to the CRI, this label is given. Often, maintainers won't have time to make such a change, but the change could be seen as good and as such the issue isn't closed.
* Make sure that ongoing PRs are moving forward at the right pace or closing them.
* In general continue to be willing to spend at least 25% of ones development time on CRI-O (~1.25
  business days per week).

## Cutting a release

* We cut releases in line with Kubernetes minor releases (1.x bump). We try to cut the release as quickly as possible after a Kubernetes release, but have been known to wait up to a month after the release, to allow for features to make it into a certain release.
* For Patch releases (1.1.z bump), releases are cut intermittantly, when there are sufficient bug fixes backported to the branch. End-users can request releases be cut, and maintainers can choose to accept that request at their discretion.
* Release notes are compiled by our [release-notes generator](https://github.com/cri-o/cri-o/blob/master/scripts/release-notes/release_notes.go).
* A release can be cut with our [release script](https://github.com/cri-o/cri-o/blob/master/scripts/release/release.go).
* Finally, create a [tagged release](https://github.com/cri-o/cri-o/releases). The release should  start with "v" and be followed by the version number. E.g., "v1.18.0". *This must match the corresponding Kubernetes release number.*

## When does a maintainer lose maintainer status

If a maintainer is no longer interested or cannot perform the maintainer duties listed above, they may volunteer to be removed from the [OWNERS list](github.com/cri-o/cri-o/tree/master/OWNERS).

In extreme cases this can also occur by a vote of the maintainers per the voting process below.

They may also opt to be come an [emeritus maintainer](#emeritus-maintainer)

# Emeritus Maintainer

An emeritus maintainer is a maintainer who used to spend a significant amount of time on the project, but now has refocused their efforts.

The CRI-O community has the emeritus maintainer tier to recognize the impact and input early maintainers have had on the project, while also being realistic about their current committment level.

There is no formal process for becoming an emeritus maintainer: a maintainer simply slows down on their contributions but does not remove themselves from the [OWNERS file](OWNERS.md).

# Feature addition policy

Features are added by simply adding a PR. The maintainers handle this the same as a bug fix PR, discussing whether the feature will be added in the PR page.

# Conflict resolution and voting

In general, we prefer that technical issues and maintainer membership are amicably worked out between the persons involved. If a dispute cannot be decided independently, the maintainers can be called in to decide an issue. If the maintainers themselves cannot decide an issue, the issue will be resolved by voting. The voting process is a simple majority in which each maintainer receives one vote. Votes not received within the span of one week are treated as an abstention.

# Adding new projects to the CRI-O GitHub organization

New projects will be added to the [CRI-O organization](github.com/cri-o) via GitHub issue discussion in one of the existing projects in the organization. Once sufficient discussion has taken place (~3-5 business days but depending on the volume of conversation), the maintainers will decide whether the new project should be added. See the section above on voting if the maintainers cannot easily decide.

# Credits

This document was extensively based off of [Envoy's GOVERNANCE.md](https://github.com/envoyproxy/envoy/blob/master/GOVERNANCE.md) file. Some credit goes to the maintainers of the Envoy project.
