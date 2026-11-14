package modelprobe

import "strings"

// Same built-in candy question as ranxi2001/sub2api, pinned to b50754c0:
// backend/internal/service/pelican_scheduled.go. The answer is a local rule,
// not a calibrated measure of intelligence or proof of a model's identity.
const taskPrompt = `在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）
苹果味 桃子味 西瓜味
圆形 7 9 8
五角星形 7 6 4`

const taskInstructions = "只输出最终答案的非负十进制整数，不要解释，不要使用 Markdown、JSON 或工具。"

func scoreAnswers(text string) (int, error) {
	answer := strings.TrimSpace(text)
	if answer == "" || len(answer) > 1 && answer[0] == '0' {
		return 0, &Error{Code: "invalid_answer_format"}
	}
	for _, r := range answer {
		if r < '0' || r > '9' {
			return 0, &Error{Code: "invalid_answer_format"}
		}
	}
	if answer == "21" {
		return 1, nil
	}
	return 0, nil
}
