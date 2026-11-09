# Product Review v1 人工核对表

作者：local-review。所有会话与问题均为虚构合成内容；不包含真实录音或个人数据。

| 问题 ID | 类型 | 问题 | 金标片段 | 金标轮次 | 结论 |
|---|---|---|---|---|---|
| prod-syn-01-q1 | synonym | 项目的预算是多少？ | seg-a1 | fact | 金标存在且唯一；金标：预算二十万元。同义改写：预算=经费。 |
| prod-syn-02-q1 | synonym | 维护费用每个月多少钱？ | seg-b1 | cost | 金标存在且唯一；金标：每月两万元。同义改写：维护费用=维护成本。 |
| prod-syn-03-q1 | synonym | 谁负责交付？ | seg-c3 | owner | 金标存在且唯一；金标：林晨。同义改写：负责交付=交付负责人。 |
| prod-syn-04-q1 | synonym | 能节省多少开支？ | seg-d2 | saving | 金标存在且唯一；金标：每年四万元。同义改写：节省=省下。 |
| prod-neg-01-q1 | negation | 方案 A 是否仍然保持不变？ | seg-b2 | switch | 金标存在且唯一；金标：改为方案 B，因此 A 不再保持。否定问法需要正确定位切换决定。 |
| prod-neg-02-q1 | negation | 第一阶段是否仍在六月完成？ | seg-b3 | timeline | 金标存在且唯一；金标：第一阶段延后到七月，因此六月完成不成立。 |
| prod-neg-03-q1 | negation | 客户是否拒绝了方案 B？ | seg-c1 | change | 金标存在且唯一；金标：客户同意方案 B，因此拒绝不成立。 |
| prod-neg-04-q1 | negation | 方案 A 的维护成本是否低于预算？ | seg-b1 | cost | 金标存在且唯一；金标：每月两万元，超出原定预算，因此低于预算不成立。 |
| prod-time-01-q1 | time_change | 项目什么时候正式启动第一阶段？ | seg-d3 | final | 金标存在且唯一；金标：七月启动第一阶段。时间变化：最初六月，后来七月。 |
| prod-time-02-q1 | time_change | 第二阶段什么时候完成？ | seg-b3 | timeline | 金标存在且唯一；金标：九月完成第二阶段。 |
| prod-time-03-q1 | time_change | 培训安排在什么时间？ | seg-c2 | training | 金标存在且唯一；金标：发布后的第二周。 |
| prod-time-04-q1 | time_change | 预算后来调整成多少？ | seg-d1 | budget | 金标存在且唯一；金标：调整为二十六万元。时间变化：最初二十万，后来二十六万。 |
| prod-cross-01-q1 | cross_session | 方案 B 是怎么被确定下来的？ | seg-b2 | switch | 金标存在且唯一；金标：改为采用方案 B，分两阶段交付。跨会话决策链。 |
| prod-cross-02-q1 | cross_session | Atlas 项目的最终决定是什么？ | seg-d3 | final | 金标存在且唯一；金标：最终敲定方案 B，预算二十六万元，七月启动。 |
| prod-cross-03-q1 | cross_session | 客户对发布时间的最后要求是什么？ | seg-c1 | change | 金标存在且唯一；金标：同意方案 B，但要求六月前完成第一阶段。 |
| prod-cross-04-q1 | cross_session | 维护成本和预算之间是什么关系？ | seg-b1 | cost | 金标存在且唯一；金标：维护成本每月两万元，超出原定预算。 |
| prod-unans-01-q1 | unanswerable | 办公室的地址在哪里？ | — | — | 语料无依据，正确行为是不返回候选 |
| prod-unans-02-q1 | unanswerable | 晚餐吃了什么？ | — | — | 语料无依据，正确行为是不返回候选 |
| prod-unans-03-q1 | unanswerable | 谁赢得了那场比赛？ | — | — | 语料无依据，正确行为是不返回候选 |
| prod-unans-04-q1 | unanswerable | 航班的编号是多少？ | — | — | 语料无依据，正确行为是不返回候选 |
| prod-unans-05-q1 | unanswerable | 明天的天气怎么样？ | — | — | 语料无依据，正确行为是不返回候选 |
| prod-unans-06-q1 | unanswerable | 车牌号码是多少？ | — | — | 语料无依据，正确行为是不返回候选 |
| prod-unans-07-q1 | unanswerable | 宠物叫什么名字？ | — | — | 语料无依据，正确行为是不返回候选 |
| prod-unans-08-q1 | unanswerable | 合同的编号是什么？ | — | — | 语料无依据，正确行为是不返回候选 |
