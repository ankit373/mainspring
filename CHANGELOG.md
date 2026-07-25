# Changelog

## 0.1.0 (2026-07-25)


### Features

* admin API — reload / drain / model load-unload ([#65](https://github.com/ankit373/mainspring/issues/65)) ([#68](https://github.com/ankit373/mainspring/issues/68)) ([31ccce1](https://github.com/ankit373/mainspring/commit/31ccce1b32d6785de37ad99b27081ddb86fda2fb))
* **auth:** per-tenant quotas + RBAC ([#9](https://github.com/ankit373/mainspring/issues/9)) ([#14](https://github.com/ankit373/mainspring/issues/14)) ([18c2faa](https://github.com/ankit373/mainspring/commit/18c2faa2b7b10461fc669bd583a10e71124ce562))
* backend health checks + circuit breaker ([#58](https://github.com/ankit373/mainspring/issues/58)) ([#61](https://github.com/ankit373/mainspring/issues/61)) ([d0d1869](https://github.com/ankit373/mainspring/commit/d0d1869c066fb1548106a5da2ac1d78e834dcf64))
* **backend:** GPT4All adopt-if-present adapter ([#35](https://github.com/ankit373/mainspring/issues/35)) ([#41](https://github.com/ankit373/mainspring/issues/41)) ([70e12d7](https://github.com/ankit373/mainspring/commit/70e12d747cab8e7d00b3f6122335928893537bbf))
* **backend:** llamafile adopt adapter + reusable openaiadopt ([#34](https://github.com/ankit373/mainspring/issues/34)) ([#40](https://github.com/ankit373/mainspring/issues/40)) ([8a1b911](https://github.com/ankit373/mainspring/commit/8a1b911c5439c578a26e005d12cea54799d3ed81))
* **backend:** LM Studio adopt-if-present adapter ([#22](https://github.com/ankit373/mainspring/issues/22)) ([#28](https://github.com/ankit373/mainspring/issues/28)) ([5140b9a](https://github.com/ankit373/mainspring/commit/5140b9ad535bc70b17a82def32474deed7085bbf))
* **backend:** MLX subprocess adapter + shared proc helpers ([#3](https://github.com/ankit373/mainspring/issues/3)) ([#16](https://github.com/ankit373/mainspring/issues/16)) ([f310fb8](https://github.com/ankit373/mainspring/commit/f310fb84c2de4dfc6bd6a290f16293f0d13a9201))
* **backend:** Ollama adopt-if-present adapter + backend selection ([#4](https://github.com/ankit373/mainspring/issues/4)) ([#15](https://github.com/ankit373/mainspring/issues/15)) ([c575498](https://github.com/ankit373/mainspring/commit/c575498ce85fa5f603d53d9fb80408d0d649c800))
* **cli:** mainspring doctor — environment diagnostics ([#36](https://github.com/ankit373/mainspring/issues/36)) ([#42](https://github.com/ankit373/mainspring/issues/42)) ([6bf1f91](https://github.com/ankit373/mainspring/commit/6bf1f910b8937ddca58f6f4c2cd8c51553cc90fc))
* **deploy:** container image + Helm chart ([#10](https://github.com/ankit373/mainspring/issues/10)) ([#18](https://github.com/ankit373/mainspring/issues/18)) ([0ee1758](https://github.com/ankit373/mainspring/commit/0ee1758973ec81c483d84cc3a5caa4b2cc5a96da))
* **deploy:** k8s ServiceMonitor + HPA + GPU values ([#26](https://github.com/ankit373/mainspring/issues/26)) ([#32](https://github.com/ankit373/mainspring/issues/32)) ([963cecf](https://github.com/ankit373/mainspring/commit/963cecf5d0e99f6f259e0d93d7e8d7a40ec08229))
* **install:** ed25519 signed-manifest verification ([#25](https://github.com/ankit373/mainspring/issues/25)) ([#31](https://github.com/ankit373/mainspring/issues/31)) ([7fb3b0e](https://github.com/ankit373/mainspring/commit/7fb3b0e6fef41da07efad0fa92b55b1f698b91d9))
* **install:** opt-in, checksum-verified managed engine install ([#6](https://github.com/ankit373/mainspring/issues/6)) ([#17](https://github.com/ankit373/mainspring/issues/17)) ([80152a3](https://github.com/ankit373/mainspring/commit/80152a35ec01cb6e0eb10c8cb94eb125a7ff7cf0))
* **observability:** TTFT, tokens/sec, /metrics, usage ledger ([#8](https://github.com/ankit373/mainspring/issues/8)) ([#13](https://github.com/ankit373/mainspring/issues/13)) ([2ff079b](https://github.com/ankit373/mainspring/commit/2ff079bf22fea086c892f62f722422ec971d65e2))
* per-model backend fallback chains + failover ([#59](https://github.com/ankit373/mainspring/issues/59)) ([#62](https://github.com/ankit373/mainspring/issues/62)) ([dcd5473](https://github.com/ankit373/mainspring/commit/dcd5473bfd657d160456c4827454e667228d8254))
* scaffold Mainspring — backend-agnostic local inference server (Phase 0) ([675546c](https://github.com/ankit373/mainspring/commit/675546c52398a11bbded84cc8524f4b6dd46ea46))
* **scheduler:** model aliases — friendly name → backend spec ([#47](https://github.com/ankit373/mainspring/issues/47)) ([#53](https://github.com/ankit373/mainspring/issues/53)) ([c009528](https://github.com/ankit373/mainspring/commit/c009528c9fb7f977a940722cda9c458ce9caa0d1))
* **scheduler:** per-model backend routing ([#21](https://github.com/ankit373/mainspring/issues/21)) ([#27](https://github.com/ankit373/mainspring/issues/27)) ([15dc289](https://github.com/ankit373/mainspring/commit/15dc2894afd3bff6a420c9945324e53def3b3ebd))
* **scheduler:** VRAM-byte-aware admission + residency ([#7](https://github.com/ankit373/mainspring/issues/7)) ([#12](https://github.com/ankit373/mainspring/issues/12)) ([fa85148](https://github.com/ankit373/mainspring/commit/fa851484f9bfbf938beedb782e4f9ad7575b6ca9))
* **serve:** graceful drain on shutdown + /readyz ([#37](https://github.com/ankit373/mainspring/issues/37)) ([#39](https://github.com/ankit373/mainspring/issues/39)) ([353bda1](https://github.com/ankit373/mainspring/commit/353bda10d4a32663e4da45181dcaaa1313cd214d))
* **serve:** model warmup / preload on startup ([#24](https://github.com/ankit373/mainspring/issues/24)) ([#30](https://github.com/ankit373/mainspring/issues/30)) ([e84ff47](https://github.com/ankit373/mainspring/commit/e84ff4737b4e93f462f26cd36cbbf0877b673891))
* **server:** /v1/quality — optional Hydra routing signal ([#51](https://github.com/ankit373/mainspring/issues/51)) ([#57](https://github.com/ankit373/mainspring/issues/57)) ([4af6bb0](https://github.com/ankit373/mainspring/commit/4af6bb0961aa36d98aba20b7ac330f47e08cdc40))
* **server:** Anthropic /v1/messages compatibility ([#33](https://github.com/ankit373/mainspring/issues/33)) ([#43](https://github.com/ankit373/mainspring/issues/43)) ([4d22fbd](https://github.com/ankit373/mainspring/commit/4d22fbdeacaab26a68bc53c0d81df62356c996aa))
* **server:** Anthropic tool-use translation ([#46](https://github.com/ankit373/mainspring/issues/46)) ([#52](https://github.com/ankit373/mainspring/issues/52)) ([b3103cf](https://github.com/ankit373/mainspring/commit/b3103cf9f5c549dda86fa8123ede5ee59050519d))
* **server:** config hot-reload on SIGHUP ([#50](https://github.com/ankit373/mainspring/issues/50)) ([#56](https://github.com/ankit373/mainspring/issues/56)) ([595be88](https://github.com/ankit373/mainspring/commit/595be883eb1587124847ce1b209deace3e679ac6))
* **server:** request concurrency limits + backpressure ([#23](https://github.com/ankit373/mainspring/issues/23)) ([#29](https://github.com/ankit373/mainspring/issues/29)) ([8dd0453](https://github.com/ankit373/mainspring/commit/8dd04538a9a58873aa5089796bfe5009aa30fdd5))
* **server:** request ID + structured access log ([#49](https://github.com/ankit373/mainspring/issues/49)) ([#55](https://github.com/ankit373/mainspring/issues/55)) ([be9e0f8](https://github.com/ankit373/mainspring/commit/be9e0f818c9bc3fcabe589e6adde7d95fa3dc0c2))
* **server:** streaming usage accounting ([#48](https://github.com/ankit373/mainspring/issues/48)) ([#54](https://github.com/ankit373/mainspring/issues/54)) ([ff16f5a](https://github.com/ankit373/mainspring/commit/ff16f5a5b4f039bf3518e9607518542ce8b55565))
* **server:** structured error taxonomy ([#60](https://github.com/ankit373/mainspring/issues/60)) ([#63](https://github.com/ankit373/mainspring/issues/63)) ([b066063](https://github.com/ankit373/mainspring/commit/b0660634c1583d1f495d583acaafc18721893bd1))
* TLS / HTTPS support ([#64](https://github.com/ankit373/mainspring/issues/64)) ([#67](https://github.com/ankit373/mainspring/issues/67)) ([5db6d7b](https://github.com/ankit373/mainspring/commit/5db6d7b534d58d3f66da54af4d7e5c5b2457d747))
* W3C trace-context propagation ([#66](https://github.com/ankit373/mainspring/issues/66)) ([#69](https://github.com/ankit373/mainspring/issues/69)) ([206277b](https://github.com/ankit373/mainspring/commit/206277b470c82705db7d378142a5cc9b2d171ead))


### Bug Fixes

* **docs:** balance section/div tags in landing page ([#38](https://github.com/ankit373/mainspring/issues/38)) ([#45](https://github.com/ankit373/mainspring/issues/45)) ([e9b9ca8](https://github.com/ankit373/mainspring/commit/e9b9ca8b1fa5cbcc9a97d47dc9af59428be43993))


### Chores

* release 0.1.0 ([bb6b276](https://github.com/ankit373/mainspring/commit/bb6b27644e2505401bea300eb77a7321291d6a21))
