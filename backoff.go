package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"time"
)

type Policy struct {
	MaxAttempts int           // 总共试几次，含第一次
	Base        time.Duration // 第一次重试等多久
	Max         time.Duration // 单次等待上限
	Factor      float64       // 每次乘多少
	Jitter      float64       // 抖动比例 0~1，防止一堆客户端同时重试
	Total       time.Duration // 总耗时上限，0 表示不限
}

func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts: 5,
		Base:        200 * time.Millisecond,
		Max:         10 * time.Second,
		Factor:      2,
		Jitter:      0.2,
	}
}

func (p Policy) validate() error {
	if p.MaxAttempts < 1 {
		return errors.New("重试次数至少是 1")
	}
	if p.Base <= 0 {
		return errors.New("初始间隔要大于 0")
	}
	if p.Factor < 1 {
		return errors.New("倍数不能小于 1，否则间隔越等越短")
	}
	if p.Jitter < 0 || p.Jitter > 1 {
		return errors.New("抖动比例要在 0 到 1 之间")
	}
	if p.Max < 0 {
		return errors.New("上限不能是负数")
	}
	return nil
}

// 第 n 次重试（从 1 开始）该等多久，不含抖动
func (p Policy) delayFor(n int) time.Duration {
	if n < 1 {
		return 0
	}
	// 指数涨得快，先用 float 算，溢出了直接顶到上限
	f := float64(p.Base) * math.Pow(p.Factor, float64(n-1))
	if math.IsInf(f, 0) || f > float64(math.MaxInt64) {
		if p.Max > 0 {
			return p.Max
		}
		return time.Duration(math.MaxInt64)
	}
	d := time.Duration(f)
	if p.Max > 0 && d > p.Max {
		d = p.Max
	}
	return d
}

// 加了抖动的等待时长
func (p Policy) delayJittered(n int, rnd *rand.Rand) time.Duration {
	d := p.delayFor(n)
	if p.Jitter <= 0 {
		return d
	}
	// 在 [d*(1-j), d*(1+j)] 里取
	spread := float64(d) * p.Jitter
	out := float64(d) + (rnd.Float64()*2-1)*spread
	if out < 0 {
		out = 0
	}
	// 抖动也不能冲破上限
	if p.Max > 0 && time.Duration(out) > p.Max {
		return p.Max
	}
	return time.Duration(out)
}

// 试到成功为止，或者次数用完
type Attempt struct {
	N     int
	Err   error
	Delay time.Duration // 这次失败后等了多久，最后一次是 0
}

type Result struct {
	Attempts []Attempt
	Err      error // 最后一次的错误，成功了是 nil
	Elapsed  time.Duration
}

func (r Result) OK() bool { return r.Err == nil }

// 返回 false 表示这个错误不用重试，直接放弃
type Retryable func(error) bool

func Do(ctx context.Context, p Policy, fn func(attempt int) error, opts ...Option) (Result, error) {
	cfg := options{
		sleep:     func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, d) },
		now:       time.Now,
		rnd:       rand.New(rand.NewSource(time.Now().UnixNano())),
		retryable: func(error) bool { return true },
	}
	for _, o := range opts {
		o(&cfg)
	}

	var res Result
	if err := p.validate(); err != nil {
		return res, err
	}

	start := cfg.now()
	for n := 1; n <= p.MaxAttempts; n++ {
		err := fn(n)
		if err == nil {
			res.Attempts = append(res.Attempts, Attempt{N: n})
			res.Elapsed = cfg.now().Sub(start)
			return res, nil
		}

		res.Err = err

		// 明确不该重试的错误，比如 404，重试多少次都一样
		if !cfg.retryable(err) {
			res.Attempts = append(res.Attempts, Attempt{N: n, Err: err})
			res.Elapsed = cfg.now().Sub(start)
			return res, err
		}
		if n == p.MaxAttempts {
			res.Attempts = append(res.Attempts, Attempt{N: n, Err: err})
			break
		}

		d := p.delayJittered(n, cfg.rnd)
		// 总时长快到了就别再等了，直接收尾
		if p.Total > 0 {
			left := p.Total - cfg.now().Sub(start)
			if left <= 0 {
				res.Attempts = append(res.Attempts, Attempt{N: n, Err: err})
				res.Elapsed = cfg.now().Sub(start)
				return res, fmt.Errorf("总时长超了 %v: %w", p.Total, err)
			}
			if d > left {
				d = left
			}
		}

		res.Attempts = append(res.Attempts, Attempt{N: n, Err: err, Delay: d})
		if cfg.onRetry != nil {
			cfg.onRetry(n, err, d)
		}
		if serr := cfg.sleep(ctx, d); serr != nil {
			res.Elapsed = cfg.now().Sub(start)
			return res, fmt.Errorf("等待被中断: %w", serr)
		}
	}

	res.Elapsed = cfg.now().Sub(start)
	return res, fmt.Errorf("试了 %d 次都失败: %w", p.MaxAttempts, res.Err)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type options struct {
	sleep     func(context.Context, time.Duration) error
	now       func() time.Time
	rnd       *rand.Rand
	retryable Retryable
	onRetry   func(n int, err error, next time.Duration)
}

type Option func(*options)

func WithRetryable(f Retryable) Option {
	return func(o *options) {
		if f != nil {
			o.retryable = f
		}
	}
}

func OnRetry(f func(n int, err error, next time.Duration)) Option {
	return func(o *options) { o.onRetry = f }
}

// 测试用：换掉 sleep 和时钟，不真等
func withClock(sleep func(context.Context, time.Duration) error, now func() time.Time) Option {
	return func(o *options) {
		if sleep != nil {
			o.sleep = sleep
		}
		if now != nil {
			o.now = now
		}
	}
}

func withRand(r *rand.Rand) Option {
	return func(o *options) {
		if r != nil {
			o.rnd = r
		}
	}
}
