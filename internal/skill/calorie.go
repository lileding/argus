package skill

// NewCalorieSkill creates the calorie tracking skill.
// The agent parses natural language food descriptions, estimates calories,
// and records them using the db tool.
func NewCalorieSkill() *Skill {
	return &Skill{
		Name:        "calorie",
		Description: "Track daily food intake and calorie consumption",
		SystemPrompt: `## 热量记录 Skill

你具备热量记录能力。当用户提到吃了什么、喝了什么，你应该：

1. 解析食物名称和大致份量
2. 估算热量（卡路里），基于常见食物热量数据
3. 使用 db 工具将记录写入 food_log 表

写入 SQL 格式：
INSERT INTO food_log (chat_id, description, calories, meal_type, logged_at)
VALUES ('{{chat_id}}', '食物描述', 估算热量, 'meal_type', NOW());

meal_type 取值：breakfast / lunch / dinner / snack
根据当前时间和上下文判断是哪一餐。

查询今日热量：
SELECT description, calories, meal_type, logged_at
FROM food_log
WHERE chat_id = '{{chat_id}}' AND logged_at::date = CURRENT_DATE
ORDER BY logged_at;

查询今日总热量：
SELECT COALESCE(SUM(calories), 0) as total
FROM food_log
WHERE chat_id = '{{chat_id}}' AND logged_at::date = CURRENT_DATE;

注意事项：
- 用户说"吃了xxx"、"喝了xxx"、"早餐吃了"等都应触发记录
- 如果用户没说份量，按常见份量估算
- 记录后回复：食物名称、估算热量、今日累计
- 用户询问"今天吃了多少"、"今日热量"等，查询并汇总回复`,
		Tools: []string{"db"},
	}
}
