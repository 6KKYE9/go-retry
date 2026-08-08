package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"time"
)

func main() {
	p := DefaultPolicy()
	flag.IntVar(&p.MaxAttempts, "n", p.MaxAttempts, "最多试几次（含第一次）")
	flag.DurationVar(&p.Base, "base", p.Base, "第一次重试等多久")
	flag.DurationVar(&p.Max, "max", p.Max, "单次等待上限")
	flag.Float64Var(&p.Factor, "factor", p.Factor, "每次间隔乘多少")
	flag.Float64Var(&p.Jitter, "jitter", p.Jitter, "抖动比例，0 到 1")
	flag.DurationVar(&p.Total, "total", 0, "总耗时上限，0 表示不限")
	plan := flag.Bool("plan", false, "只打印各次的等待时长，不真跑")
	quiet := flag.Bool("q", false, "不打印重试过程")

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `go-retry - 指数退避重试

失败就重试地跑一条命令:
  go-retry -n 5 -base 1s -- curl -f https://example.com

只看退避计划不真跑:
  go-retry -n 6 -base 200ms -factor 2 -plan

参数:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *plan {
		printPlan(p)
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	// Ctrl+C 能立刻打断等待，不用干等完退避时间
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var opts []Option
	if !*quiet {
		opts = append(opts, OnRetry(func(n int, err error, next time.Duration) {
			fmt.Fprintf(os.Stderr, "第 %d 次失败(%v)，%v 后重试\n", n, err, next.Round(time.Millisecond))
		}))
	}

	res, err := Do(ctx, p, func(n int) error {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		return cmd.Run()
	}, opts...)

	if !*quiet {
		fmt.Fprintf(os.Stderr, "共试了 %d 次，用时 %v\n", len(res.Attempts), res.Elapsed.Round(time.Millisecond))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "最终失败:", err)
		os.Exit(1)
	}
}

func printPlan(p Policy) {
	if err := p.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "出错:", err)
		os.Exit(1)
	}
	fmt.Printf("最多 %d 次，起步 %v，倍数 %.2f，上限 %v，抖动 %.0f%%\n\n",
		p.MaxAttempts, p.Base, p.Factor, p.Max, p.Jitter*100)

	var sum time.Duration
	for n := 1; n < p.MaxAttempts; n++ {
		d := p.delayFor(n)
		sum += d
		mark := ""
		if p.Total > 0 && sum > p.Total {
			mark = "  <- 这里已经超过总时长上限"
		}
		fmt.Printf("第 %d 次失败后等 %-10v 累计 %v%s\n",
			n, d.Round(time.Millisecond), sum.Round(time.Millisecond), mark)
	}
	fmt.Printf("\n最坏情况总等待 %v（不含命令本身耗时）\n", sum.Round(time.Millisecond))
	if p.Jitter > 0 {
		lo := time.Duration(float64(sum) * (1 - p.Jitter))
		hi := time.Duration(float64(sum) * (1 + p.Jitter))
		fmt.Printf("算上抖动大概在 %v ~ %v 之间\n", lo.Round(time.Millisecond), hi.Round(time.Millisecond))
	}
}
