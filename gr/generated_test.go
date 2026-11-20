package gr

import (
	"encoding/json"
	"testing"
)

func TestBookStatsAcceptFractionalRatingsSum(t *testing.T) {
	var stats BookInfoStatsBookOrWorkStats
	if err := json.Unmarshal([]byte(`{"averageRating":4.5,"ratingsCount":10,"ratingsSum":287479.5}`), &stats); err != nil {
		t.Fatalf("fractional ratingsSum must decode: %v", err)
	}
	if stats.RatingsSum != 287479.5 {
		t.Fatalf("unexpected ratingsSum: %v", stats.RatingsSum)
	}
}
