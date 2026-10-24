import { MarkdownStream, StreamingMarkdown } from '@ycj3/streaming-markdown';
import type { StreamingMarkdownConfig } from '@ycj3/streaming-markdown';
import { StaticMarkdownView } from './StaticMarkdown';
import { DARK_PALETTE, LIGHT_PALETTE } from '../common/Constants';

/**
 * MarkdownView — unified markdown renderer for chat messages.
 *
 * - isStreaming=true  → uses @ycj3/streaming-markdown live StreamingMarkdown
 *                      (incremental, animated — ideal for SSE output).
 * - isStreaming=false → uses StaticMarkdownView (parses once, no timers,
 *                      instant render — ideal for history / finalized messages).
 */
@Component
export struct MarkdownView {
  @Prop content: string = '';
  @Prop isStreaming: boolean = false;
  @Prop isDark: boolean = true;

  private stream: MarkdownStream = new MarkdownStream({ mode: 'word', interval: 18 });
  private seeded: boolean = false;
  private pushedLen: number = 0;

  aboutToAppear(): void {
    if (this.isStreaming) {
      this.seedStream();
      this.seeded = true;
    }
  }

  // Triggered by @Prop changes (streaming updates)
  private onContentChange(): void {
    if (this.isStreaming && this.seeded) {
      const c: string = this.content;
      const prev: number = this.pushedLen;
      if (c.length > prev) {
        const delta: string = c.substring(prev);
        this.stream.append(delta);
        this.pushedLen = c.length;
        return;
      }
      // non-prefix change → reseed
      this.seedStream();
    }
  }

  private seedStream(): void {
    this.stream.reset();
    this.pushedLen = 0;
    const c: string = this.content;
    if (c.length > 0) {
      this.stream.append(c);
      this.pushedLen = c.length;
    }
    // Streaming message stays open; finish happens when isStreaming flips false.
  }

  /**
   * When a streaming message finalizes, mark the stream complete.
   */
  private onStreamingEnd(): void {
    if (this.seeded) {
      this.stream.finish();
    }
  }

  private getConfig(): StreamingMarkdownConfig {
    const p = this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
    const config: StreamingMarkdownConfig = {
      heading: {
        sizes: [28, 24, 20, 18, 16, 14],
        color: p.textPrimary,
        topSpacing: 8,
        bottomSpacing: 6,
      },
      paragraph: {
        fontSize: 15,
        lineHeight: 24,
        color: p.msgBubbleText,
        bottomSpacing: 6,
      },
      list: {
        fontSize: 15,
        lineHeight: 24,
        itemBottomSpacing: 3,
        color: p.msgBubbleText,
      },
      blockquote: {
        textColor: p.textTertiary,
        bgColor: p.bgHover,
        borderColor: p.kvBorder,
        bottomSpacing: 6,
      },
      codeBlock: {
        borderColor: p.kvBorder,
        radius: 10,
        topSpacing: 8,
        bottomSpacing: 8,
      },
      table: {
        headerBgColor: p.bgHover,
        borderColor: p.kvBorder,
        stripeBgColor: p.bgHover,
        cellFontSize: 13,
        topSpacing: 6,
        bottomSpacing: 6,
      },
      horizontalRule: {
        color: p.kvBorder,
        topSpacing: 8,
        bottomSpacing: 8,
      },
      inline: {
        linkColor: p.accent,
        codeTextColor: p.preText,
        codeBgColor: p.preBg,
        mathTextColor: p.accent,
        mathBgColor: p.bgHover,
        monoFontFamily: 'monospace',
      },
      layout: {
        contentPadding: { left: 0, right: 0, top: 0, bottom: 0 },
      },
    };
    return config;
  }

  build() {
    // 布局说明（已由 uitest dumpLayout 实测确认）：
    // 父气泡 BubbleBody 为内容自适应宽度（constraintSize maxWidth 78%）且带 12vp 左右 padding。
    // 在这种"内容自适应 + padding"的父节点下，后代节点的 width('100%') 会被解析成父气泡的
    // 外框宽度而不是内容框宽度，于是 markdown 内容整体右溢出 12vp 并被气泡 clip 裁掉。
    // 解决办法：用 Row + layoutWeight(1) 代替百分比宽度。layoutWeight 走的是"剩余约束分配"
    // 而不是百分比解析，能拿到正确的内容框宽度，再把这个确定宽度传给 StaticMarkdown，
    // 其内部各层的 width('100%') 就有了正确的解析基准。
    Row() {
      Column() {
        if (this.isStreaming) {
          StreamingMarkdown({
            stream: this.stream,
            config: this.getConfig(),
          })
        } else {
          StaticMarkdownView({
            content: this.content,
            isDark: this.isDark,
          })
        }
      }
      .layoutWeight(1)
      .alignItems(HorizontalAlign.Start)
    }
    .alignItems(VerticalAlign.Top)
  }
}
