# ISVC 落地 Qwen3.8-27B 发现的 API 缺口

> 记录日期：2026-09-07
> 背景：把已验证的 LWS 多机部署 manifest（`manifests/qwen3.8-27b-fp16.yaml`，TP8×PP2、2×8 MetaX C500）
> 迁移到 ISVC（ModelVersion + InferenceRuntimeProfile + InferenceService）时发现。
> 当前策略：模板层 workaround 绕过，**先把单机 8 卡 case 跑通**，再按本文清单修 operator。
> 模板文件：`manifests/qwen38-27b-fp16-isvc.yaml`（多机）、`manifests/qwen38-27b-fp16-isvc-single-node.yaml`（单机）。
> 2026-09-07 晚补充：单机 variant 首次 apply 发现问题 5（mount 型 asset 不挂载，Pod exit 127），
> 临时以方案 B 绕过（entrypoint 内联进 command，新建 Profile `metax-vllm-qwen38-tp8-inline` 切 profileRef）。

## 问题清单

### 1. `podTemplate.volumes` 生成孤儿卷（bug，优先级最高）

**现象**：Profile 里声明的附加卷（emptyDir / hostPath）只被加进 `spec.Volumes`，从不生成容器
`volumeMounts`——容器根本看不到这个卷。

**证据**：
- `operator/internal/controller/podspec.go` `buildPodSpec`：`container.VolumeMounts` 只来自
  `modelVolumeMounts(pt.Mounts, model)`；`pt.Volumes` 仅 append 到 `spec.Volumes`。
- `operator/internal/controller/podspec_test.go` "converts additional volumes..." 测试只断言了
  `spec.Volumes`，没有断言容器挂载——所以该缺陷没有被测试拦住。
- API 层也缺字段：`api/v1alpha1/inferenceruntimeprofile_types.go` 的 `Volume` 只有
  `{name, emptyDir, hostPath}`，没有挂载路径字段，即使想挂也无处表达。

**影响**：无法挂载 `/dev/shm`、InfiniBand 设备等附加卷。vLLM TP>1 的进程内 SHM transport 需要
大于容器默认 64Mi 的 `/dev/shm`，原 manifest 用 `emptyDir{medium: Memory, sizeLimit: 8Gi}` 提供。

**修复建议**：
- `Volume` 增加 `at` 字段（容器内挂载路径，`Pattern="^/"` 必填），`buildPodSpec` 为每个附加卷生成
  对应 `volumeMount`。
- 补 `podspec_test.go` 断言：容器 `VolumeMounts` 包含每个附加卷的挂载点。

### 2. `EmptyDirVolume` 不支持 `medium` / `sizeLimit`

**现象**：`EmptyDirVolume` 是空结构体，渲染为 `emptyDir: {}`。已验证 manifest 的
`emptyDir{medium: Memory, sizeLimit: 8Gi}`（tmpfs + 限额）无法表达——即使修好问题 1 也一样。

**修复建议**：`EmptyDirVolume` 增加 `medium`（enum: `""|Memory`）与 `sizeLimit`（Quantity）两个
可选字段，透传进 `corev1.EmptyDirVolumeSource`。

### 3. `PodResources` 不支持 GPU 以外的扩展资源

**现象**：`resources` 只有 `{cpu, memory, gpuPerPod}`。已验证 manifest 请求
`rdma/hca_shared_devices: "2"`（requests+limits），无法表达。

**影响**：仅多机场景——Pod 内 RDMA 依赖设备插件按请求分配 HCA，缺失则跨 Pod PP 通信无法走 RDMA；
调度器也不对 HCA 记账。单机 variant 无影响（无跨 Pod 通信）。

**修复建议**：`PodResources` 增加 `extendedResources map[string]int64`，同时写入 requests 和
limits（与 `gpuPerPod` 语义一致）。注意校验 key 不能是 `cpu`/`memory`/厂商 GPU 资源名，避免与
现有字段冲突或被用来绕过 GPU 记账。

### 4. `PodTemplate` 无 affinity（设计取舍，暂可接受）

**现象**：无法表达原 manifest 的 leader/worker `podAntiAffinity`。

**评估**：8 卡/Pod + 8 卡/节点时，GPU 请求本身已保证组内 Pod 异机调度，缺口被天然覆盖；
仅当节点 GPU 数 > `gpuPerPod`（如同节点可塞下两个组）时才有共置风险。单机 variant 无影响。

**结论**：记录观察即可，暂不改 API；未来若需要再加 `affinity` 受控子集。

### 5. mount 型 asset 不挂载进 Pod（bug，优先级最高——单机部署 exit 127 的直接原因）

**发现**：2026-09-07 晚，单机 variant 首次 apply 后 Pod `Error, Exit Code 127`（`/bin/bash: /scripts/start.sh: No such file or directory`）。

**现象**：`assets[].mount` 声明的 ConfigMap 副本被渲染并复制到服务 namespace（`provisionAssets`，
ISVC `status.assets` 有回显，Pod 注解 `template-hash-assets` 有值），但 Pod 渲染从不生成对应的
volume/volumeMount——副本成了孤儿对象，容器里脚本不存在。

**证据**：
- `operator/internal/controller/podspec.go` `buildPodSpec`：容器 `VolumeMounts` 只来自
  `modelVolumeMounts(pt.Mounts, model)`；`pt.EnvFromAssets` 只生成 envFrom。mount 型 asset 在
  Pod 渲染路径上完全缺席；全仓库 grep 无 `ConfigMapVolumeSource` / `defaultMode` 的使用。
- 与设计契约冲突：`api.md` §assets[] 与 §5.1 渲染契约——"mount 类型的副本以
  `defaultMode: <mode>` 挂载到声明的路径，对所有 role 生效"。
- 实例：describe 显示容器仅挂载 `/models` 与 serviceaccount token，无 `/scripts`。
- 与问题 1（podTemplate.volumes 孤儿卷）是同族缺陷但相互独立：本问题不依赖 volumes 声明。

**影响**：所有使用 `assets[].mount`（entrypoint 脚本模式）的 profile 都会 127；且无模板层绕法——
`podTemplate.volumes` 只支持 emptyDir/hostPath（CEL 强制二选一），表达不了 ConfigMap 卷；
Profile spec 不可变（VAP），`command` 无法原地修改。

**修复建议**：
- `desiredWorkload`/`buildPodSpec` 为每个 mount 型 asset 生成（对所有 role 生效，参照
  `addCredentialsVolume` 的注入模式）：volume `{name: asset-<asset>, configMap: {name: <isvc>-<asset>,
  defaultMode: <mount.mode>}}` + volumeMount `{name: asset-<asset>, mountPath: <mount.path>, readOnly: true}`。
- 补测试：podspec 单测断言 asset volume+volumeMount；`inferenceservice_controller_test.go` 现有
  asset fixture（mount `/opt/bootstrap`）补 workload 模板断言。
- 命名约定 `asset-<name>` 需与 `pt.Volumes`/credentials 卷防撞（校验或文档化）。
- 修复后单机 variant 回退 assets 方式（见验收标准 4）。

### 6. `accelerator.models` 调度约束未注入（设计 L0 行为实现暂缓；启用条件已具备）

**现象**：profile 声明 `accelerator.models: [MXC500]`，渲染出的 Pod `Node-Selectors: <none>`——
混合型号集群（C500/C550 都提供 `metax-tech.com/gpu`）里，针对 C500 验证的 profile 可能被调度到
C550 节点（api.md §3.2：引擎 JIT 编译错架构、显存配置不匹配、启动一段时间后才暴露问题）。

**背景**：api.md §3.2 设计了自动注入（单型号 nodeSelector / 多型号 nodeAffinity `In`，与
`podTemplate.nodeSelector` AND 合并），但以"GPU 型号 label 来源未定"为由暂缓；约定的平台 label
`ai.cubestack.io/accelerator-model` 实际无人写入。

**2026-09-07 事实来源确认**：MetaX 设备插件栈已在节点写入稳定的厂商原生 label：
`metax-tech.com/gpu.product=MXC500`、`gpu.family=MXC`、`gpu.memory=64GB`、
`gpu.driver.ready/maca.ready/runtime.ready=true`、驱动版本等。NVIDIA 侧 GPU Feature Discovery
有对等物（`nvidia.com/gpu.product` 等）。厂商原生 label 与扩展资源名（`metax-tech.com/gpu` /
`nvidia.com/gpu`）同属设备插件写入，同一信任域——可直接作为事实来源。

**修复建议**：
- controller 按 vendor→label 映射注入：metax → `metax-tech.com/gpu.product`，nvidia →
  `nvidia.com/gpu.product`；单型号 → nodeSelector，多型号 → nodeAffinity `In`（语义照 api.md §3.2）。
- 与 `podTemplate.nodeSelector` 合并：注入 key 与管理员声明的 key 冲突（同 key 不同值）时在
  Rendered 阶段报错，避免渲染出永不可调度的 Pod。
- 同步更新 api.md §3.2：写入事实来源结论（厂商原生 label），替换 `ai.cubestack.io/accelerator-model` 约定。
- 验收：单机 variant 渲染出的 Pod `Node-Selectors` 含 `metax-tech.com/gpu.product=MXC500`。

### 7. 网关配置没有部署层入口（bug——功能完整但按标准部署方式不可用）

**发现**：2026-09-07，单机 variant 验证 `route.publish` 时，`kubectl -n cubestack-system get deploy
cubestack-controller-manager -o yaml | grep gateway-` 无输出——operator 运行在 flag 默认值
（`GatewayName=""`、`GatewayDomain=""`）上。

**现象**：InferenceServiceReconciler 支持三个 flag `--gateway-name/--gateway-namespace/
--gateway-domain`（route.go 发布 HTTPRoute 的必要输入），但两种部署方式都没有入口：
- kustomize（`config/manager/manager.yaml`）：args 写死 `--metrics-bind-address/--leader-elect/
  --health-probe-bind-address`，无 gateway 三项；
- helm chart（`templates/deployment-cubestack-controller-manager.yaml`）：同样写死，values.yaml
  无对应键。

**影响**：任何按标准方式部署的集群，`spec.route.publish: true` 必然 `RouteReady=False,
reason=GatewayNotConfigured`——网关发布功能存在但永远无法按设计路径启用。实测只能手动
patch deployment args 绕过（且 helm upgrade 会冲掉手改）。

**修复建议**：
- helm chart：values 增加 `gateway.name/gateway.namespace/gateway.domain`（空值 = 不传 flag，
  保持现状语义），模板按值注入 args。
- kustomize：config/manager 同步（或示例值）。
- 连带修：`status.endpoint.public` 硬编码 `"https://"+hostname`（inferenceservice_controller.go:240），
  与网关 listener 实际协议无关——至少文档化为"规范地址"，或按 listener 协议生成。
- 验收：`helm install` 后不手动 patch，`publish: true` 直接走通到 `RouteReady=True`。

## 已应用的模板层 workaround（operator 修复后应回退）

| workaround | 位置 | 回退条件 |
|---|---|---|
| `securityContext.privileged: true` | 两个模板文件的 profile `podTemplate`，及方案 B 内联 Profile | 问题 1+2 修复后移除 |
| 脚本内 `mount -t tmpfs -o size=8G tmpfs /dev/shm` | 两个 asset ConfigMap 的 `start.sh`；方案 B 内联进 command | 同上；改回 `volumes` 声明 dshm + `medium: Memory` + `sizeLimit: 8Gi` |
| entrypoint 内联进 `podTemplate.command`（方案 B：新建 Profile `metax-vllm-qwen38-tp8-inline` + ISVC 切 `profileRef`；临时 apply，manifest 未落盘） | 单机部署 | 问题 5 修复后：新建干净 Profile（assets entrypoint + volumes dshm），切回 profileRef，删除 inline Profile |

注意：移除 workaround 属于 Profile 内容变更 → spec 不可变，需要新建版本化 Profile 名并切换
`profileRef`（正常升级流程）。

## 非缺陷但容易踩坑（写 Profile/asset 时注意）

- **`{{ model.path }}` 是 role 级变量，asset data 中不可用**（renderer 报 PhaseViolation）。
  正确姿势：podTemplate `env: [{name: MODEL_PATH, value: "{{ model.path }}"}]`，脚本用
  `${MODEL_PATH}` 引用。asset data 只允许服务级变量（`model.name/version/architecture/quantization`、
  `service.*`、`overrides.*`、`profile.vars.*`、`route.*`）。
- **asset 源 ConfigMap 必须**在 `cubestack-system`、`immutable: true`、名称以 `-vX.Y.Z` 结尾
  （VAP 强制），否则 `Resolved=False, reason=AssetNotFound`。
- **served-model-name 渲染为 `{{ model.name }}`**：isvc `publish: true` 时 `route.modelName`
  必须等于 `ModelVersion.spec.model`，否则客户端请求 404。
- `ModelVersion` 命名绑定 `metadata.name == spec.model + '-' + spec.version`；Profile 命名前缀
  `<vendor>-<engine>-`；两者 spec 均不可变（VAP）。
- **`accelerator.models` 当前不注入调度约束（describe 里 `Node-Selectors: <none>`）**：实现暂缓的
  设计行为，已升格为问题 6（label 事实来源已确认，具备启用条件）。启用前多节点池场景需管理员在
  `podTemplate.nodeSelector` 手动写等价约束（如 `metax-tech.com/gpu.product: MXC500`）；单节点集群无影响。
- **QoS 显示 BestEffort**：`gpuPerPod` 映射的是扩展资源（`metax-tech.com/gpu`），K8s QoS 只按
  cpu/memory 计算，GPU-only requests 的 Pod 就是 BestEffort——非缺陷，describe 里看着刺眼而已。
- **readiness 预算要覆盖最坏加载时间**：MetaX 插件把 `VLLM_ENGINE_READY_TIMEOUT_S` 设为 7200s
  （厂商知道大模型加载慢），而 readiness 预算 = initialDelay + failureThreshold×periodSeconds，
  本模板 60s+180×10s≈31min 就会杀掉仍在加载的容器。大模型/慢存储场景必须调大
  `failureThreshold`（按最坏冷读时间估算），否则"健康但慢"的加载被反复杀死重启、永远起不来。
  实测教训：2026-09-07 单机 27B fp16 加载期间节点出现 OSD 下线 + GPU 卡 PCIe AER 下线，
  卡死 30min 后由探针兜底重启——基础设施故障最终会以"探针杀 Pod"的形式浮出，日志静默期
  需要下到节点看 `dmesg`/`mx-smi` 才能定位根因。
- **节点重启后 GPU Pod 进入 `UnexpectedAdmissionError`**：kubelet device manager 无法按重启前
  的旧分配重新准入容器（设备分配 checkpoint 失效）。实测（2026-09-07 单机）：StatefulSet 控制器
  会把 Failed phase 的 Pod 判为不活跃并**自动删除重建**（无需人工干预），重建后 device plugin
  重新分配；裸 Pod 才需要手动删。注意重建时机依赖 device plugin 已重新发布设备——节点刚起、
  插件未就绪时重建可能再失败，等插件就绪或手动删一次即可。GPU 节点重启是运维 GPU 卡瞬态
  故障（如 PCIe AER）的标准手段。
- **HostPath 策略要求节点侧存储挂载持久化**：模型预分发目录（如节点手动 mount 的 CephFS）
  若没写 `/etc/fstab`（`_netdev,nofail`），节点重启后挂载即丢；Pod 卡在 kubelet 的 hostPath
  目录检查（`ContainerCreating`），平台层（ISVC 状态）没有任何提示——hostPath 目录就绪是
  管理员责任（api.md §3.1 预分发语义的隐性部分）。节点准备脚本必须包含持久挂载。
  GPU 节点重启恢复 SOP：重启 → `mx-smi` 确认 8/8 → CephFS 重挂载（fstab 化后自动）→
  `delete pod`（UnexpectedAdmissionError）→ 冷读重放。

- **网关栈是 Envoy AI Gateway（extension manager 形态），xDS 翻译硬依赖 ai-gateway-controller**：
  集群的 `envoy-gateway-config` 里 `extensionManager.service` 指向
  `ai-gateway-controller.ai-gateway-system:1063`，且 hook 为 `includeAll: true` +
  post-Translation/Cluster/Route——**所有**路由（含 cubestack 发布的普通 HTTPRoute）的 xDS
  翻译都经过它回调。实测（2026-09-07）：非 AI 路由可正确 passthrough（NodePort 全链路验证通过）；
  但 controller 不可用时网关**配置更新会冻结**（数据面继续服务存量配置）——它是关键组件，
  升级/监控需与 envoy-gateway 同级对待。架构决策：平台发布原语保持标准 HTTPRoute（operator
  契约不绑 AI Gateway 的 0.x CRD）；token 限流/usage 记账/schema 翻译等 AI 能力未来以
  AIGatewayRoute 挂同一 Gateway 或 EG 原生策略的增量方式引入，不改推理服务的接入路径。

## 修复后的验收标准

1. 移除 privileged + remount workaround 后，单机 variant 渲染出的 Pod spec 中：
   `volumeMounts` 含 `/dev/shm`，卷为 `emptyDir{medium: Memory, sizeLimit: 8Gi}`。
2. 多机模板渲染出的 Pod spec 中：`resources` 含 `rdma/hca_shared_devices: "2"`（requests+limits）。
3. 对照原 `manifests/qwen3.8-27b-fp16.yaml` 逐项 diff，除命名/标签外的运行时配置全部等价。
4. 问题 5 修复后：以 `assets[].mount` 方式渲染的单机 Pod spec 中，`volumeMounts` 含 `/scripts`
   （configMap 卷 `qwen38-27b-entrypoint`，`defaultMode: 0755`），entrypoint 回到 asset ConfigMap，
   方案 B inline Profile 退役。
5. `make test` 通过（含新增断言）；`make lint` 无新增问题。
