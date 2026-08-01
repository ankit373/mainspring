# Changelog

## [0.2.0](https://github.com/ankit373/mainspring/compare/v0.1.0...v0.2.0) (2026-08-01)


### Features

* **admin:** GET /admin/config — effective runtime settings (secret-free) ([#96](https://github.com/ankit373/mainspring/issues/96)) ([f97b9e3](https://github.com/ankit373/mainspring/commit/f97b9e39d28f73946e43bbbb69e71ca0fdaf73b2)), closes [#93](https://github.com/ankit373/mainspring/issues/93)
* **admin:** POST /admin/breaker/{id}/reset — manual circuit-breaker reset ([#115](https://github.com/ankit373/mainspring/issues/115)) ([35ea8e9](https://github.com/ankit373/mainspring/commit/35ea8e90d737189e872f4eb09af447b7a0070de0)), closes [#112](https://github.com/ankit373/mainspring/issues/112)
* **admin:** POST /admin/cache/clear — manual response-cache purge ([#116](https://github.com/ankit373/mainspring/issues/116)) ([ee10256](https://github.com/ankit373/mainspring/commit/ee1025634e5f00b7dc03c17da3eb81111cfa6344)), closes [#113](https://github.com/ankit373/mainspring/issues/113)
* **admin:** report interrupted in-flight requests on forced unload ([#134](https://github.com/ankit373/mainspring/issues/134)) ([4bd9a4b](https://github.com/ankit373/mainspring/commit/4bd9a4bf4fb94c51d513b63e51f9e3543207099b)), closes [#131](https://github.com/ankit373/mainspring/issues/131)
* adopt-backend model auto-discovery ([#75](https://github.com/ankit373/mainspring/issues/75)) ([#78](https://github.com/ankit373/mainspring/issues/78)) ([a52fc8c](https://github.com/ankit373/mainspring/commit/a52fc8c4380668dadde6fcf2c4d2ede1ade7a98d))
* **auth:** rate-limit and token-budget headroom headers ([#128](https://github.com/ankit373/mainspring/issues/128)) ([6b1061a](https://github.com/ankit373/mainspring/commit/6b1061a0e1b86d1dd6ad8860b700be16a4e7670e)), closes [#125](https://github.com/ankit373/mainspring/issues/125)
* **backend/ollama:** EstimateMemory via /api/tags size field ([#127](https://github.com/ankit373/mainspring/issues/127)) ([986482e](https://github.com/ankit373/mainspring/commit/986482e12c865ab9d422a61f016cf8b85bd4860f)), closes [#124](https://github.com/ankit373/mainspring/issues/124)
* **cache:** opt-in TTL+LRU response cache for deterministic requests ([#81](https://github.com/ankit373/mainspring/issues/81)) ([d6d8970](https://github.com/ankit373/mainspring/commit/d6d897018e4a1af0c158076656690e9c5c84b24e)), closes [#80](https://github.com/ankit373/mainspring/issues/80)
* **config:** shared config-lint for silently-inert settings ([#120](https://github.com/ankit373/mainspring/issues/120)) ([b2a35cb](https://github.com/ankit373/mainspring/commit/b2a35cbfe3c5a7dd19bbb326e92d17eed879b30b)), closes [#117](https://github.com/ankit373/mainspring/issues/117)
* **cost:** per-model USD cost accounting from real token usage ([#84](https://github.com/ankit373/mainspring/issues/84)) ([2978c1d](https://github.com/ankit373/mainspring/commit/2978c1dc94c2f65bc4d85127a1323092f3dad77e)), closes [#82](https://github.com/ankit373/mainspring/issues/82)
* **dispatch:** model-level fallback chain when primary is unavailable ([#90](https://github.com/ankit373/mainspring/issues/90)) ([430209e](https://github.com/ankit373/mainspring/commit/430209ee42f01f058265565738abfa2b485a0481)), closes [#87](https://github.com/ankit373/mainspring/issues/87)
* **metrics:** count retries, coalesced hits, and fallbacks ([#95](https://github.com/ankit373/mainspring/issues/95)) ([9025193](https://github.com/ankit373/mainspring/commit/90251937b9e9b631eeaa47b91708f46047669080)), closes [#92](https://github.com/ankit373/mainspring/issues/92)
* **metrics:** duration + TTFT percentiles (p50/p90/p99) ([#114](https://github.com/ankit373/mainspring/issues/114)) ([2f5b5ee](https://github.com/ankit373/mainspring/commit/2f5b5eeade2b97e6173f765e4b5e79f2f188930d)), closes [#111](https://github.com/ankit373/mainspring/issues/111)
* **metrics:** per-tenant usage rollup + GET /admin/usage ([#97](https://github.com/ankit373/mainspring/issues/97)) ([388dbba](https://github.com/ankit373/mainspring/commit/388dbba6ef089367757c5bc621a4bf070a20bc6c)), closes [#94](https://github.com/ankit373/mainspring/issues/94)
* **metrics:** queue wait-time percentiles (p50/p90/p99) ([#121](https://github.com/ankit373/mainspring/issues/121)) ([8a2d20e](https://github.com/ankit373/mainspring/commit/8a2d20ef687694164cd02efc732be1c5e8e23dd4)), closes [#118](https://github.com/ankit373/mainspring/issues/118)
* per-model request timeout ([#76](https://github.com/ankit373/mainspring/issues/76)) ([#79](https://github.com/ankit373/mainspring/issues/79)) ([dfd84c6](https://github.com/ankit373/mainspring/commit/dfd84c69e72054f6dd51d50ca26037ca31c6dc47))
* **server:** /readyz reflects total outage (all breakers open) ([#122](https://github.com/ankit373/mainspring/issues/122)) ([8ebad1d](https://github.com/ankit373/mainspring/commit/8ebad1dbb0725aa1fead30c99d8f82ac9b860ea2)), closes [#119](https://github.com/ankit373/mainspring/issues/119)
* **server:** Anthropic image / multimodal content translation ([#74](https://github.com/ankit373/mainspring/issues/74)) ([#77](https://github.com/ankit373/mainspring/issues/77)) ([147cdf2](https://github.com/ankit373/mainspring/commit/147cdf238a7b7a95a8ae71837c8b620ad35a05d8))
* **server:** clamp_max_tokens — fit max_tokens to the context window ([#102](https://github.com/ankit373/mainspring/issues/102)) ([a8b886b](https://github.com/ankit373/mainspring/commit/a8b886b637f9c575691d821edf2a2700d8fc092b)), closes [#99](https://github.com/ankit373/mainspring/issues/99)
* **server:** coalesce identical in-flight requests (single-flight) ([#91](https://github.com/ankit373/mainspring/issues/91)) ([29f1657](https://github.com/ankit373/mainspring/commit/29f16573ef18a0ed1003c9dc1382877e22c1e197)), closes [#88](https://github.com/ankit373/mainspring/issues/88)
* **server:** effective-context guardrail — reject over-context requests ([#85](https://github.com/ankit373/mainspring/issues/85)) ([4f25960](https://github.com/ankit373/mainspring/commit/4f25960eadd8ac6c0654e763a590addffb639a4d)), closes [#83](https://github.com/ankit373/mainspring/issues/83)
* **server:** GET /v1/models/{id} — model detail endpoint ([#108](https://github.com/ankit373/mainspring/issues/108)) ([21fdb22](https://github.com/ankit373/mainspring/commit/21fdb228cd48eca15deb961ef88b28293932c9ec))
* **server:** precise_context — exact guardrail tokens for resident models ([#103](https://github.com/ankit373/mainspring/issues/103)) ([d95f7e1](https://github.com/ankit373/mainspring/commit/d95f7e195a10b9817cd6e4c277f634c79b434959)), closes [#100](https://github.com/ankit373/mainspring/issues/100)
* **server:** retry transient upstream failures with backoff ([#89](https://github.com/ankit373/mainspring/issues/89)) ([284ac70](https://github.com/ankit373/mainspring/commit/284ac707b165e20b60846482a5995f1b16ace84f)), closes [#86](https://github.com/ankit373/mainspring/issues/86)
* **tokenize:** backend token counter + POST /v1/tokenize ([#101](https://github.com/ankit373/mainspring/issues/101)) ([3cb274c](https://github.com/ankit373/mainspring/commit/3cb274c2aef8b1d86c5372c8b933ebd3bc1a5a81)), closes [#98](https://github.com/ankit373/mainspring/issues/98)


### Bug Fixes

* **anthropic:** request stream_options.include_usage so streaming gets exact token usage ([#107](https://github.com/ankit373/mainspring/issues/107)) ([cdf1782](https://github.com/ankit373/mainspring/commit/cdf1782c9058b1ee7457a336c9c324cee410e71b)), closes [#105](https://github.com/ankit373/mainspring/issues/105)
* **backend:** Degraded() falsely flags intentional CPU-only models ([#138](https://github.com/ankit373/mainspring/issues/138)) ([59a3c28](https://github.com/ankit373/mainspring/commit/59a3c28192a609f68bef1953c10247785f7adc02)), closes [#135](https://github.com/ankit373/mainspring/issues/135)
* **context:** count embeddings input field in guardrail and tokenizer ([#109](https://github.com/ankit373/mainspring/issues/109)) ([f5ba431](https://github.com/ankit373/mainspring/commit/f5ba431a38bdb0bc621056830726ba78b6b837d0)), closes [#106](https://github.com/ankit373/mainspring/issues/106)
* **reload:** warn about config fields that are silently ignored on reload ([#133](https://github.com/ankit373/mainspring/issues/133)) ([a15fc7a](https://github.com/ankit373/mainspring/commit/a15fc7abf970cd23a8301f2285b17f61a84c4fc3)), closes [#130](https://github.com/ankit373/mainspring/issues/130)
* **scheduler:** config reload can kill a runner mid-request ([#132](https://github.com/ankit373/mainspring/issues/132)) ([b9adf23](https://github.com/ankit373/mainspring/commit/b9adf231a81a4cb9e35980abde671bd1c97a427c)), closes [#129](https://github.com/ankit373/mainspring/issues/129)
* **scheduler:** idle-unload can kill a runner mid-request ([#126](https://github.com/ankit373/mainspring/issues/126)) ([f988eec](https://github.com/ankit373/mainspring/commit/f988eec498283e7d88aff1233f8835cb4aa11993)), closes [#123](https://github.com/ankit373/mainspring/issues/123)

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
