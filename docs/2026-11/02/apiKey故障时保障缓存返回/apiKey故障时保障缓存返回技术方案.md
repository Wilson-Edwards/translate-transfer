# DeepSeek API Key 故障时保障单词缓存返回技术方案

## 1. 背景与目标

DeepSeek API Key 过期、被撤销、额度耗尽或配置丢失时，上游通常会返回 401/403 等错误。这类故障不应影响已经保存在 **word_cache** 表中的单词翻译。

本方案目标：

- 对可缓存的单词请求，必须先查缓存；命中后立即返回，不读取或校验 API Key，也不发起上游请求。
- 流式和非流式请求在缓存命中时都保持现有 OpenAI 兼容响应格式。
- 只有缓存未命中时才进入 API Key 和 DeepSeek 上游逻辑。
- 用自动化测试固化“缓存命中时上游调用次数必须为 0”这一约束。

本需求不保证缓存未命中的单词或句子在 API Key 故障时仍能翻译，它们仍依赖上游服务。

## 2. 现状分析

当前 Go 请求链路为：

~~~text
routes.go
  -> translationProxy（handlers.go：解析请求、提取并规范化单词）
  -> buildOutgoingPayload（payload.go：构造上游 payload）
  -> proxyDeepSeek（proxy.go：查缓存、调用上游、回写缓存）
  -> getCachedWord/cacheWordTranslation（database.go）
~~~

当前 **proxyDeepSeek** 已经将单词缓存查询放在 API Key 读取和 **http.Client.Do** 之前，因此代码顺序上已有“缓存命中不调用上游”的基础。不过这项行为还没有被清晰的接口和回归测试保护，存在以下风险：

1. **getCachedWord** 只返回 **(content, found)**，数据库查询失败与真正的缓存未命中都表现为 **found=false**，无法区分和监控。
2. 缓存响应、API Key 解析和上游代理混合在一个方法内，后续修改容易破坏执行顺序。
3. 现有 **proxy_test.go** 只验证缓存 SSE 格式，没有覆盖“空/错误 API Key + 缓存命中”的完整链路，也没有断言上游未被调用。
4. **cacheEnabled()** 在 **localConfig == nil** 时返回 false，与 README 中“Cache 默认为 true”的语义不一致。配置文件丢失或解析失败时，即使数据库可用也会跳过缓存。
5. **handlers.go** 会为 Zotero JSON 单词请求额外查询一次缓存，但该结果仅用于日志；进入 **proxyDeepSeek** 后还会再次查询。

“数据库中已有缓存但请求没有返回缓存”不一定是 API Key 错误本身导致，还应检查：

- 请求是否被 **isWord** 识别为长度 1～50 的英文字母单词；
- **Relay.Cache** 是否开启；
- MySQL 是否可读；
- 请求的 **(target_language, normalized_word)** 是否与缓存记录一致。

## 3. 总体设计

将 **proxyDeepSeek** 的分支顺序固定为：

~~~text
解析请求并规范化单词
        |
        v
是单词且缓存开启？ -- 否 --> 解析 API Key --> 调用上游
        |
        | 是
        v
按 (target_language, normalized_word) 查询缓存
        |
        +-- 命中 --> 直接写非流式 JSON 或 SSE --> 结束
        |
        +-- 未命中 --> 解析 API Key --> 调用上游 --> 成功后写缓存
        |
        +-- 查询错误 --> 记录 cache_error --> 按明确的降级策略处理
~~~

必须保持以下不变式：

> 只要缓存命中，后续代码就不得读取 API Key、不得构造上游 HTTP 请求，也不得因 API Key 状态改变响应。

### 3.1 缓存查询改为三态结果

将 **database.go** 中的方法签名由：

~~~go
func (a *App) getCachedWord(targetLanguage, word string) (string, bool)
~~~

调整为：

~~~go
func (a *App) getCachedWord(
    ctx context.Context,
    targetLanguage string,
    word string,
) (content string, found bool, err error)
~~~

实现要求：

- 使用请求的 **c.Request.Context()**，不再使用 **context.Background()**，让客户端断开时可取消数据库查询。
- 先查 **sync.Map**，再查 MySQL；内存命中立即返回。
- MySQL 有记录时返回 **(content, true, nil)**，无记录时返回 **("", false, nil)**，查询异常时返回 **("", false, err)**。
- 查询继续使用 **target_language** 和小写、去空格后的 **word**，与现有唯一索引一致。
- 空翻译内容不作为有效命中，并记录数据质量告警。

数据库查询错误不应再记为 **cache_miss**。推荐默认采用 fail-open：记录 **cache_error** 后继续尝试上游，以便 API Key 正常时仍可提供服务。如果运维要求缓存故障时不得把流量转给上游，可改为返回 **503 cache_unavailable**，但两种情况都不能混入“缓存未命中”的语义。

### 3.2 封装缓存响应

在 **proxy.go** 增加统一的缓存响应方法：

~~~go
func (a *App) writeCachedResponse(
    c *gin.Context,
    payload map[string]interface{},
    content string,
)
~~~

该方法只根据 **payload["stream"]** 选择现有 **writeCachedStream** 或非流式 JSON 响应，写完后由调用方立即 return。

兼容性要求：

- 非流式：HTTP 200；**choices[0].message.role=assistant**；content 为缓存内容。
- 流式：HTTP 200；Content-Type 为 **text/event-stream**；包含 assistant role、缓存内容和结束块；最后以 **data: [DONE]** 结束。
- 缓存响应不返回 API Key 或上游错误，也不因上游故障降级为非 200。

### 3.3 API Key 逻辑仅存在于缓存未命中分支

将 API Key 解析收敛到一个小方法，并保证它在缓存分支之后才被调用：

~~~go
func (a *App) deepSeekAPIKey() string {
    if a.localConfig != nil {
        if key := strings.TrimSpace(a.localConfig.Relay.APIKey); key != "" {
            return key
        }
    }
    return strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
}
~~~

空 API Key 时，不再向上游发送空的 Bearer Token，而是只对缓存未命中的请求返回稳定的 **503 Service Unavailable**：

~~~json
{
  "error": "upstream_unavailable",
  "details": "DeepSeek API key is not configured"
}
~~~

对“非空但已过期或被撤销”的 Key，本地无法预判，缓存未命中时仍会收到上游 401/403。这不会影响缓存命中的请求，因为它们已经在发起上游请求前返回。

不建议把“收到上游 401 后再次查询相同缓存”作为主方案：缓存已在请求开始时查询，重复查询只会增加数据库压力，并可能掩盖缓存键或数据异常。

### 3.4 明确缓存开关语义

**Relay.Cache** 建议采用以下语义：

- 显式配置 false：关闭缓存读写，API Key 故障时不提供缓存保障。
- 显式配置 true 或未填写：开启缓存。
- **localConfig == nil**：为与 README 的“默认开启”保持一致，**cacheEnabled()** 返回 true。

本需求推荐保留“默认开启”，避免 API Key 或配置文件问题意外关闭已有缓存。如果产品决定改为默认关闭，则必须同步修改 README，并在启动时输出明确告警。

## 4. 关键伪代码

~~~go
func (a *App) proxyDeepSeek(
    c *gin.Context,
    payload map[string]interface{},
    targetLanguage string,
    word string,
) {
    cacheEnabled := a.cacheEnabled()

    // 缓存是前置主路径，与 API Key 完全解耦。
    if word != "" && cacheEnabled {
        content, found, err := a.getCachedWord(
            c.Request.Context(),
            targetLanguage,
            word,
        )
        if err != nil {
            log.Printf(
                "cache lookup failed: language=%s word=%q err=%v",
                targetLanguage,
                word,
                err,
            )
            // 默认 fail-open：继续上游，但不能记为 cache miss。
        } else if found {
            a.writeCachedResponse(c, payload, content)
            return
        }
    }

    // 只有非单词、缓存关闭、未命中或缓存查询失败才会到达这里。
    apiKey := a.deepSeekAPIKey()
    if apiKey == "" {
        c.JSON(http.StatusServiceUnavailable, gin.H{
            "error":   "upstream_unavailable",
            "details": "DeepSeek API key is not configured",
        })
        return
    }

    // 保留现有上游请求、流式转发与成功后写缓存逻辑。
    a.proxyUpstream(c, payload, targetLanguage, word, apiKey, cacheEnabled)
}
~~~

**proxyUpstream** 可以在本次改造中从 **proxyDeepSeek** 拆出，也可以暂时保留在原方法中。无论是否拆分，缓存命中分支必须在 API Key 解析和任何 HTTP 请求之前 return。

## 5. 改造范围

| 文件 | 改造内容 |
| --- | --- |
| **database.go** | **getCachedWord** 增加 Context 和 error 返回值；区分命中、未命中和查询失败；统一缓存键规范化；明确 **cacheEnabled()** 默认值。 |
| **proxy.go** | 保持缓存前置；增加 **writeCachedResponse** 和 **deepSeekAPIKey**；空 Key 仅在未命中分支返回 503；可选拆出 **proxyUpstream**。 |
| **handlers.go** | 保持单词规范化和 targetLanguage 传递；删除 Zotero JSON 中只用于日志的重复缓存查询。 |
| **proxy_test.go** | 新增空/错误 Key 下缓存命中的流式、非流式回归测试，并断言上游调用次数为 0。 |
| **database_test.go** | 新增缓存三态、大小写归一化、语言隔离和查询错误测试。 |
| **README.md** | 说明缓存命中不依赖 API Key，以及保障的生效条件与边界。 |

本方案不需要修改 **word_cache** 表结构或现有 HTTP API。

## 6. 测试方案

### 6.1 单元测试

使用 **httptest.Server** 作为上游，在 handler 中用 **atomic.Int32** 统计请求次数。缓存命中用例结束后必须断言计数为 0；这比只断言响应内容更能证明缓存已与 API Key 解耦。

| 场景 | 预期状态 | 预期内容 | 上游调用次数 |
| --- | ---: | --- | ---: |
| 非流式，缓存命中，API Key 为空 | 200 | JSON 中为缓存译文 | 0 |
| 非流式，缓存命中，API Key 明确无效 | 200 | JSON 中为缓存译文 | 0 |
| 流式，缓存命中，API Key 为空或无效 | 200 | SSE 含缓存译文且以 [DONE] 结束 | 0 |
| 缓存未命中，API Key 为空 | 503 | upstream_unavailable | 0 |
| 缓存未命中，上游判定 Key 无效 | 透传上游 401/403 | 上游错误体 | 1 |
| 缓存命中，但 Relay.Cache=false | 由上游决定 | 不使用缓存 | 1 |
| 中文缓存命中，但请求 Tagalog | 由上游决定 | 不得返回中文缓存 | 1 |
| MySQL 查询失败 | 按 fail-open 由上游决定 | 日志/指标为 cache_error | 1 |

“API Key 明确无效”用例不需要访问真实 DeepSeek；给 App 配置任意无效字符串，并由本地假上游统计是否被调用即可。

### 6.2 MySQL 集成测试

事故场景是“数据库已有缓存，但进程重启后内存缓存为空”，因此必须至少有一条集成测试真正查询 MySQL，不能只预热 **sync.Map**：

1. 向测试库插入 **(target_language='zh-CN', word='example')** 缓存记录。
2. 创建新 App 实例，确保其 **sync.Map** 为空，模拟进程重启。
3. 将 API Key 设置为空或无效值，请求 **example**。
4. 断言返回 MySQL 中的译文、HTTP 状态为 200、假上游调用次数为 0。
5. 分别以 **stream=false** 和 **stream=true** 执行。

## 7. 可观测性与故障定位

建议每个请求只记录一次结构化结果，不再为了日志额外查询缓存：

- **translation_source=memory_cache|database_cache|upstream**
- **cache_result=hit|miss|error|disabled|not_applicable**
- **target_language=zh-CN|tl**
- **stream=true|false**
- **upstream_status=200|401|403|...**（仅实际调用上游时记录）

建议监控：

- 单词缓存命中率；
- 缓存查询错误数；
- 上游 401/403 次数及告警；
- 空 API Key 被本地拒绝的请求数；
- 缓存命中后上游调用数，该值必须始终为 0。

不得在日志、错误响应或指标标签中输出 API Key。

## 8. 发布与验证

本改造无数据库结构变更，可直接随 Go 服务发布：

1. 先合入单元测试和 MySQL 集成测试，运行 **go test ./...**。
2. 在预发环境准备一条已知缓存，将 API Key 替换为无效值。
3. 对同一单词分别发起流式和非流式请求，确认均返回 200 及缓存译文。
4. 请求一个未缓存单词，确认仅该请求返回上游鉴权错误。
5. 查看日志和指标，确认缓存命中请求的上游调用数为 0。
6. 恢复正常 API Key，完成常规冒烟测试后发布生产。

如需回滚，只需回滚 Go 应用版本，无需回滚数据库。

## 9. 验收标准

- API Key 为空、无效或上游返回 401/403 时，已存在于 MySQL 的单词缓存仍返回 HTTP 200。
- 流式与非流式缓存响应均与当前客户端协议兼容。
- 缓存命中请求的上游调用次数为 0。
- 缓存未命中时保留现有上游调用和成功回写缓存能力。
- 中文与 Tagalog 仍按 **(target_language, word)** 隔离，不得串用缓存。
- 缓存查询错误与真正未命中可在日志或指标中区分。
