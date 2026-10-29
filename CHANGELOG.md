# Changelog

## [0.1.1](https://github.com/xnilsit/robot-ptr/compare/0.1.0...0.1.1) (2026-10-29)


### Features

* skip nodes that are not Hetzner Robot servers ([#6](https://github.com/xnilsit/robot-ptr/issues/6)) ([5ea9a64](https://github.com/xnilsit/robot-ptr/commit/5ea9a64a89518e3c26ccae2799bb3bba62a2e12f))


### Continuous Integration

* **github-action:** update action jdx/mise-action (v5.2.3 ➔ v5.2.4) ([#2](https://github.com/xnilsit/robot-ptr/issues/2)) ([bfab875](https://github.com/xnilsit/robot-ptr/commit/bfab8753b40f5be587a45a325d73fad582333f80))
* **github-action:** update action jdx/mise-action (v5.2.3 ➔ v5.2.4) ([#5](https://github.com/xnilsit/robot-ptr/issues/5)) ([8683dca](https://github.com/xnilsit/robot-ptr/commit/8683dca8d995e906e2cd0955e63e3d237bb19b49))
* let the security release merge wait for CI ([387ef83](https://github.com/xnilsit/robot-ptr/commit/387ef83ff04e8f7df5107eccf425ed27e5f41276))
* release fixes for high-severity vulnerabilities right away ([8953b8f](https://github.com/xnilsit/robot-ptr/commit/8953b8f1694726109b7935893dfe694b50b6500a))


### Miscellaneous Chores

* **renovate:** automerge vulnerability fixes ([8ca4a65](https://github.com/xnilsit/robot-ptr/commit/8ca4a65cfc8640195ac79e429f8e13d89750b8bb))

## 0.1.0 (2026-10-29)


### Features

* add a Helm chart published as OCI artifact ([b51ad13](https://github.com/xnilsit/robot-ptr/commit/b51ad133103178fec1246e6b606417c8887ce09b))
* initial prototype of the robot-ptr controller ([86dfb43](https://github.com/xnilsit/robot-ptr/commit/86dfb43437f9f79062b19a456c9ad4832d752893))


### Bug Fixes

* **deps:** update golang.org/x/net to v0.62.0 ([f0d118d](https://github.com/xnilsit/robot-ptr/commit/f0d118ddb7527f106be0f3a472f4f29b622683fa))
* keep the reset delay across controller restarts ([19385e6](https://github.com/xnilsit/robot-ptr/commit/19385e6b84ea944021daddfc0267fe48a69d89bd))
* record ConflictingPTR events only when the conflict changes ([ed09838](https://github.com/xnilsit/robot-ptr/commit/ed098389eb0402236a9e84500c859336ea0b4497))


### Performance Improvements

* cache only the pod fields the controller reads ([f5c7e36](https://github.com/xnilsit/robot-ptr/commit/f5c7e366a740eff488ea84191eaac77455f829dd))


### Code Refactoring

* rename annotation domain to nilsit.dev ([80741ca](https://github.com/xnilsit/robot-ptr/commit/80741ca172cf3f709efe6cea8ac38f2351b11f72))


### Documentation

* license under Apache-2.0 ([e6ff360](https://github.com/xnilsit/robot-ptr/commit/e6ff3605a15833c7f882232654023e38dcb27904))


### Continuous Integration

* bump the chart version and appVersion through the generic updater ([b261364](https://github.com/xnilsit/robot-ptr/commit/b261364085aa8a08560665a7b134d8ff85d2e474))
* only open release PRs for releasable commits ([66af3de](https://github.com/xnilsit/robot-ptr/commit/66af3de2358854df440edf57577e88a92ec2d27b))
* run release-please with a GitHub App token ([f48be98](https://github.com/xnilsit/robot-ptr/commit/f48be987749ae911970f4406dd21c376d92a99c5))
* start releases at 0.1.0 ([2f38c1a](https://github.com/xnilsit/robot-ptr/commit/2f38c1a597130aeaaf627498a31a1ed93f1ba0d2))
