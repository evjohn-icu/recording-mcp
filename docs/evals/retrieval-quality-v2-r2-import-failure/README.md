# R2 导入失败记录

全部运行在导入第一条过期干扰修订时失败：`turn obsolete has missing or duplicate original_index`。没有执行任何检索评分，test也未执行。原R2冻结字节保留；R3仅修正ASR original_index为合法的正整数并进行真实导入预检，语义文本、问题和金标不变。已增加该字段的失败优先回归测试与冻结前校验。
