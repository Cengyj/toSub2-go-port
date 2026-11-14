package modelprobe

import (
	"strings"
	"testing"
)

func TestCandyAnswerFromAllPossibleShapeAndFlavorCounts(t *testing.T) {
	// Enumerate every handful without the required cross-shape apple/peach
	// pair. A shape quota guarantees success only when no such handful exists.
	var canFail [25][18]bool
	blindFailureMaximum := 0
	for roundApple := 0; roundApple <= 7; roundApple++ {
		for roundPeach := 0; roundPeach <= 9; roundPeach++ {
			for roundWatermelon := 0; roundWatermelon <= 8; roundWatermelon++ {
				for starApple := 0; starApple <= 7; starApple++ {
					for starPeach := 0; starPeach <= 6; starPeach++ {
						for starWatermelon := 0; starWatermelon <= 4; starWatermelon++ {
							if roundApple > 0 && starPeach > 0 || roundPeach > 0 && starApple > 0 {
								continue
							}
							round := roundApple + roundPeach + roundWatermelon
							star := starApple + starPeach + starWatermelon
							canFail[round][star] = true
							if round+star > blindFailureMaximum {
								blindFailureMaximum = round + star
							}
						}
					}
				}
			}
		}
	}
	minimum := 42
	for round := range canFail {
		for star, failure := range canFail[round] {
			if !failure && round+star < minimum {
				minimum = round + star
			}
		}
	}
	if minimum != 21 || canFail[9][12] || blindFailureMaximum+1 != 29 {
		t.Fatalf("shape-aware minimum=%d, nine-round/twelve-star fails=%t, blind minimum=%d", minimum, canFail[9][12], blindFailureMaximum+1)
	}
	if score, err := scoreAnswers("21"); err != nil || score != 1 {
		t.Fatalf("independently solved answer was not accepted: %d %v", score, err)
	}
	if score, err := scoreAnswers("29"); err != nil || score != 0 {
		t.Fatalf("shape-blind answer was not a valid failed observation: %d %v", score, err)
	}
}

func TestCandyPromptPreservesTheDistinguishableShapesAndTable(t *testing.T) {
	for _, text := range []string{"不同的形状靠手感可以分辨", "苹果味 桃子味 西瓜味", "圆形 7 9 8", "五角星形 7 6 4"} {
		if !strings.Contains(taskPrompt, text) {
			t.Fatalf("candy question lost its premise %q", text)
		}
	}
	if strings.Contains(taskPrompt, "21") || strings.Contains(taskInstructions, "21") || strings.Contains(taskInstructions, "JSON object") {
		t.Fatal("question leaked its answer or retained the old six-question delivery format")
	}
	if RuleVersion == "reasoning-six-v1" || TotalQuestions != 1 || DefaultMinimumScore != 1 {
		t.Fatal("new candy result could be confused with an old six-question cache entry")
	}
}

func TestCandyIntegerScoringUsesTheWholeFinalAnswer(t *testing.T) {
	for _, tc := range []struct {
		answer string
		score  int
	}{
		{"21", 1}, {" \n21\t", 1}, {"29", 0}, {"129", 0}, {"210", 0}, {"20", 0}, {"0", 0},
	} {
		if score, err := scoreAnswers(tc.answer); err != nil || score != tc.score {
			t.Fatalf("answer %q => %d,%v; want %d", tc.answer, score, err, tc.score)
		}
	}
}
