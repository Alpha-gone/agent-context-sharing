package main

import "math"

// criticalValue는 Student t 분포의 0.975 분위수다. 자유도 1의 값이 16보다 작고
// 자유도가 커질수록 감소하므로, 이 구간에서 이분법으로 양측 95% 임계값을 구한다.
// CDF와 불완전 베타의 관계 및 연분수는 reference.md의 NIST·DLMF 근거를 따른다.
func criticalValue(degrees int) float64 {
	if degrees < 1 {
		return undefinedMetric
	}
	low, high := 0.0, 16.0
	for range 60 {
		middle := (low + high) / 2
		x := middle * middle / (float64(degrees) + middle*middle)
		cdf := 0.5 + 0.5*regularizedBeta(x, 0.5, float64(degrees)/2)
		if math.IsNaN(cdf) {
			return undefinedMetric
		}
		if cdf < 0.975 {
			low = middle
		} else {
			high = middle
		}
	}
	return (low + high) / 2
}

func regularizedBeta(x, a, b float64) float64 {
	if x == 0 || x == 1 {
		return x
	}
	lab, _ := math.Lgamma(a + b)
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	factor := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return factor * betaFraction(a, b, x) / a
	}
	return 1 - factor*betaFraction(b, a, 1-x)/b
}

// betaFraction은 수정 Lentz 방법으로 연분수를 평가한다. 분모의 0과 비수렴을
// 제한하고, 실패를 NaN으로 전달해 정의되지 않은 구간으로 처리한다.
func betaFraction(a, b, x float64) float64 {
	clamp := func(value float64) float64 {
		if math.Abs(value) < 1e-300 {
			return math.Copysign(1e-300, value)
		}
		return value
	}
	c := 1.0
	d := 1 / clamp(1-(a+b)*x/(a+1))
	result := d
	for m := 1; m <= 512; m++ {
		value := float64(m)
		aa := value * (b - value) * x / ((a + 2*value - 1) * (a + 2*value))
		d = 1 / clamp(1+aa*d)
		c = clamp(1 + aa/c)
		result *= d * c
		aa = -(a + value) * (a + b + value) * x / ((a + 2*value) * (a + 2*value + 1))
		d = 1 / clamp(1+aa*d)
		c = clamp(1 + aa/c)
		delta := d * c
		result *= delta
		if math.Abs(delta-1) < 3e-14 {
			return result
		}
	}
	return math.NaN()
}
