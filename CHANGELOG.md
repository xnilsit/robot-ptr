# Changelog

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

* license under Apache-2.0 ([01a0ee9](https://github.com/xnilsit/robot-ptr/commit/01a0ee909d1fd6fafe99cce275928171c6da938c))
* license under Apache-2.0 ([e6ff360](https://github.com/xnilsit/robot-ptr/commit/e6ff3605a15833c7f882232654023e38dcb27904))


### Continuous Integration

* bump the chart version and appVersion through the generic updater ([b261364](https://github.com/xnilsit/robot-ptr/commit/b261364085aa8a08560665a7b134d8ff85d2e474))
* only open release PRs for releasable commits ([66af3de](https://github.com/xnilsit/robot-ptr/commit/66af3de2358854df440edf57577e88a92ec2d27b))
* run release-please with a GitHub App token ([f48be98](https://github.com/xnilsit/robot-ptr/commit/f48be987749ae911970f4406dd21c376d92a99c5))
* start releases at 0.1.0 ([2f38c1a](https://github.com/xnilsit/robot-ptr/commit/2f38c1a597130aeaaf627498a31a1ed93f1ba0d2))
