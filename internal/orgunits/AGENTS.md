# orgunits：部门投影

`mdm/org` 的部门在本组件的投影（装配项目协议 02），只为一件事：部门维度（安全 07）的组织树路径。单据把自己部门的路径复制到 `department_path`，列表按这一列下推维度；部门移动时它的事件改写这些副本。

## 负责与不负责

- 负责：订阅 `org-unit-events`（`OnOrgUnit`：按聚合；不认识的动作标为不完整，修复工作项取回）；按需补取（`Department`）；三个后台工作：每小时的 `align-org-units`、`repair-org-units`、组织换了纪元（整套恢复之后）立即的 `realign-org-units`（表 `org_realign` 去重）；改写报价、订单、交货的 `department_path`（`repath`）。
- 不负责：单据取哪个部门（`documents` 的 `departmentOf`）；维度怎样编译进查询（SDK 的列表）。

## 文件

| 文件 | 内容 |
| --- | --- |
| `orgunits.go` | 上面全部：`repath`、`OnOrgUnit`、`Department`、投影的来源 `orgSource`（`mdm/org` 的 `SourceService`）、`realignSoon`、三个工作 |

## 依赖与被依赖

- 依赖：`core`、`masterdata`（`OrgUnit`、`OrgUnitRead`）。
- 被依赖：`documents`（`Department`）；入口登记订阅与工作。

## 规则在哪

装配项目协议 02（投影；PJ 编号）、线协议 P6.33（以后的动作）；`AGENTS.md`（仓库根）“设计取舍”的“部门路径复制到单据上”。

## 测试

`itest/orgunits_integration_test.go`（`departments`：部门移到另一部门之下，它的订单跟着走；`laterOrgEvents`：不认识的动作与同一流上的法人事件，PJ36）。
