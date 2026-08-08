package main

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// 假时钟 + 假 sleep，单测不真等
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

func fakeOpts(c *fakeClock) []Option {
	return []Option{
		withClock(c.Sleep, c.Now),
		withRand(rand.New(rand.NewSource(1))),
	}
}

var boom = errors.New("炸了")

func TestSucceedFirstTry(t *testing.T) {
	c := newClock()
	calls := 0
	res, err := Do(context.Background(), DefaultPolicy(), func(n int) error {
		calls++
		return nil
	}, fakeOpts(c)...)

	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("成功了不该重试，调了 %d 次", calls)
	}
	if !res.OK() {
		t.Error("结果该是成功")
	}
}

func TestRetryUntilSuccess(t *testing.T) {
	c := newClock()
	calls := 0
	_, err := Do(context.Background(), DefaultPolicy(), func(n int) error {
		calls++
		if calls < 3 {
			return boom
		}
		return nil
	}, fakeOpts(c)...)

	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("该在第 3 次成功，实际调了 %d 次", calls)
	}
}

// 次数用完要包住原始错误，方便上层 errors.Is
func TestExhaustWrapsErr(t *testing.T) {
	c := newClock()
	p := DefaultPolicy()
	p.MaxAttempts = 3
	calls := 0
	_, err := Do(context.Background(), p, func(n int) error {
		calls++
		return boom
	}, fakeOpts(c)...)

	if err == nil {
		t.Fatal("全失败该返回错误")
	}
	if !errors.Is(err, boom) {
		t.Errorf("该能 unwrap 到原始错误: %v", err)
	}
	if calls != 3 {
		t.Errorf("该调 3 次，实际 %d 次", calls)
	}
}

// 间隔要按倍数涨
func TestDelayGrows(t *testing.T) {
	p := Policy{MaxAttempts: 5, Base: time.Second, Factor: 2, Max: time.Hour}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := p.delayFor(i + 1); got != w {
			t.Errorf("第 %d 次该等 %v，得到 %v", i+1, w, got)
		}
	}
}

// 涨到上限就不再涨了
func TestDelayCapped(t *testing.T) {
	p := Policy{MaxAttempts: 20, Base: time.Second, Factor: 2, Max: 5 * time.Second}
	for n := 5; n <= 20; n++ {
		if got := p.delayFor(n); got != 5*time.Second {
			t.Errorf("第 %d 次该被压到 5s，得到 %v", n, got)
		}
	}
}

// 次数多了 float 会 Inf，转 Duration 会变成负数，得挡住
func TestDelayNoOverflow(t *testing.T) {
	p := Policy{MaxAttempts: 2000, Base: time.Second, Factor: 10, Max: time.Minute}
	for _, n := range []int{100, 500, 1000, 1999} {
		d := p.delayFor(n)
		if d <= 0 {
			t.Errorf("第 %d 次算出了非正数 %v，八成是溢出了", n, d)
		}
		if d > time.Minute {
			t.Errorf("第 %d 次冲破上限: %v", n, d)
		}
	}
	// 不设上限时也不能溢出成负数
	p.Max = 0
	if d := p.delayFor(1000); d <= 0 {
		t.Errorf("不设上限也不该算出负数: %v", d)
	}
}

// 抖动不能把等待时间弄成负的，也不能冲破上限
func TestJitterBounds(t *testing.T) {
	p := Policy{MaxAttempts: 10, Base: time.Second, Factor: 2, Max: 4 * time.Second, Jitter: 0.5}
	r := rand.New(rand.NewSource(42))
	for i := 0; i < 2000; i++ {
		for n := 1; n <= 8; n++ {
			d := p.delayJittered(n, r)
			if d < 0 {
				t.Fatalf("抖出了负数: %v", d)
			}
			if d > p.Max {
				t.Fatalf("抖动冲破了上限: %v > %v", d, p.Max)
			}
		}
	}
}

// 抖动为 0 时结果必须是确定的
func TestNoJitterDeterministic(t *testing.T) {
	p := Policy{MaxAttempts: 5, Base: time.Second, Factor: 2, Max: time.Hour, Jitter: 0}
	r := rand.New(rand.NewSource(7))
	for n := 1; n <= 4; n++ {
		if p.delayJittered(n, r) != p.delayFor(n) {
			t.Errorf("抖动关掉了还在抖，第 %d 次", n)
		}
	}
}

// 不该重试的错误要立刻放弃
func TestNonRetryableStopsEarly(t *testing.T) {
	c := newClock()
	fatal := errors.New("参数不对，重试也没用")
	calls := 0
	opts := append(fakeOpts(c), WithRetryable(func(err error) bool {
		return !errors.Is(err, fatal)
	}))

	_, err := Do(context.Background(), DefaultPolicy(), func(n int) error {
		calls++
		return fatal
	}, opts...)

	if calls != 1 {
		t.Errorf("不该重试的错误只该调 1 次，实际 %d 次", calls)
	}
	if !errors.Is(err, fatal) {
		t.Errorf("该原样返回这个错误: %v", err)
	}
}

// 总时长到了就得停，不能因为次数没用完继续等
func TestTotalBudget(t *testing.T) {
	c := newClock()
	p := Policy{MaxAttempts: 100, Base: time.Second, Factor: 2, Max: time.Minute, Total: 5 * time.Second}
	calls := 0
	_, err := Do(context.Background(), p, func(n int) error {
		calls++
		return boom
	}, fakeOpts(c)...)

	if err == nil {
		t.Fatal("该失败")
	}
	if calls >= 100 {
		t.Errorf("总时长该提前刹车，却调了 %d 次", calls)
	}
	if elapsed := c.Now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); elapsed > 5*time.Second {
		t.Errorf("等的时间超预算了: %v", elapsed)
	}
}

// ctx 取消要立刻中断，不用等完退避
func TestCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := Do(ctx, DefaultPolicy(), func(n int) error {
		calls++
		if n == 2 {
			cancel()
		}
		return boom
	}, withClock(func(c context.Context, d time.Duration) error { return c.Err() }, time.Now))

	if err == nil {
		t.Fatal("取消后该返回错误")
	}
	if calls > 2 {
		t.Errorf("取消后还在重试，调了 %d 次", calls)
	}
}

func TestOnRetryCallback(t *testing.T) {
	c := newClock()
	p := DefaultPolicy()
	p.MaxAttempts = 4
	var seen []int
	opts := append(fakeOpts(c), OnRetry(func(n int, err error, next time.Duration) {
		seen = append(seen, n)
		if next <= 0 {
			t.Errorf("第 %d 次回调里的等待时长不该是 %v", n, next)
		}
	}))

	Do(context.Background(), p, func(n int) error { return boom }, opts...)

	// 4 次尝试只有前 3 次后面跟着等待
	if len(seen) != 3 {
		t.Errorf("回调该触发 3 次，实际 %d 次", len(seen))
	}
}

func TestBadPolicy(t *testing.T) {
	cases := []Policy{
		{MaxAttempts: 0, Base: time.Second, Factor: 2},
		{MaxAttempts: 3, Base: 0, Factor: 2},
		{MaxAttempts: 3, Base: time.Second, Factor: 0.5},
		{MaxAttempts: 3, Base: time.Second, Factor: 2, Jitter: 1.5},
		{MaxAttempts: 3, Base: time.Second, Factor: 2, Max: -time.Second},
	}
	for i, p := range cases {
		if _, err := Do(context.Background(), p, func(int) error { return nil }); err == nil {
			t.Errorf("第 %d 个非法策略该被拦下", i)
		}
	}
}

func TestAttemptRecord(t *testing.T) {
	c := newClock()
	p := DefaultPolicy()
	p.MaxAttempts = 3
	res, _ := Do(context.Background(), p, func(n int) error { return boom }, fakeOpts(c)...)

	if len(res.Attempts) != 3 {
		t.Fatalf("该记 3 条，得到 %d", len(res.Attempts))
	}
	// 最后一次后面不再等待
	if res.Attempts[2].Delay != 0 {
		t.Errorf("最后一次不该有等待: %v", res.Attempts[2].Delay)
	}
	for i, a := range res.Attempts {
		if a.N != i+1 {
			t.Errorf("序号乱了: %+v", a)
		}
	}
}

func TestDelayForZeroAndNegative(t *testing.T) {
	p := DefaultPolicy()
	if p.delayFor(0) != 0 || p.delayFor(-5) != 0 {
		t.Error("非正的次数该返回 0")
	}
}

func TestFactorOneIsFlat(t *testing.T) {
	p := Policy{MaxAttempts: 5, Base: time.Second, Factor: 1, Max: time.Hour}
	for n := 1; n <= 4; n++ {
		if p.delayFor(n) != time.Second {
			t.Errorf("倍数为 1 该一直是固定间隔，第 %d 次得到 %v", n, p.delayFor(n))
		}
	}
}
