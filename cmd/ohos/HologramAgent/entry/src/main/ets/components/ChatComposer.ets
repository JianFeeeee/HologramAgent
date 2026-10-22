/**
 * 悬浮输入区：加号菜单 + 待发送附件预览 + 输入行（含发送/中断）。
 *
 * 从 pages/ChatPage.ets 抽出（原来是 ChatBody 的层2 + AttachMenu /
 * AttachOption / PendingAttachmentChip 三个 @Builder，外加选图/选文件、
 * 沙箱落盘、带附件发送这一整套方法）。
 *
 * 输入区自己持有文本与附件状态；只有会影响【列表底部留白】的三项
 * （inputMultiLine / attachMenuOpen / pendingName）用 @Link 与页面共享。
 */

import { ChatMessage } from '../model/Model';
import { userMessage } from '../common/UserError';
import { connStore } from '../common/ConnStore';
import { ThemePalette, DARK_PALETTE, LIGHT_PALETTE, ANIM_FAST, ANIM_NORMAL } from '../common/Constants';
import { mimeOf } from '../common/ChatFormat';
import { chatStore, K_CHAT_LOADING } from '../common/ChatStore';
import { sendChatText, sendChatFile, interruptChat } from '../common/ChatSession';
import { fileNameOf } from '../common/AttachmentMeta';
import { ChatAttachMenu, ChatPendingChip } from './ChatAttachBar';
import { NavFloatOverlay, FloatIconButton } from './PageTopBar';
import { picker, fileIo } from '@kit.CoreFileKit';
import { common } from '@kit.AbilityKit';
import { MeasureOptions } from '@ohos.measure';

/** 输入框字号与内边距：文字测量必须和 TextArea 的实际排版参数一致 */
const INPUT_FONT_SIZE: number = 14;
const INPUT_INNER_PAD: number = 16;
/** 单行态左右让位：左边加号 42+8，右边发送键 44+8 */
const INPUT_LEFT_GAP: number = 50;
const INPUT_RIGHT_GAP: number = 52;

@Component
export struct ChatComposer {
  @StorageProp('themeIsDark') private isDark: boolean = true;
  @StorageProp(K_CHAT_LOADING) private loading: boolean = false;
  /** 输入框是否已进入多行态（页面用它算列表底部留白） */
  @Link inputMultiLine: boolean;
  /** 加号菜单是否展开（同上） */
  @Link attachMenuOpen: boolean;
  /** 待发送附件的展示名（同上） */
  @Link pendingName: string;
  @State inputText: string = '';
  @State pendingSize: number = 0;
  @State pendingIsImage: boolean = false;
  @State uploading: boolean = false;
  private pendingPath: string = '';
  private pendingMime: string = '';
  /** 底部固定行的实测宽度：用于文字测量，判断是否需要换行 */
  private inputRowWidth: number = 0;

  private palette(): ThemePalette {
    return this.isDark ? DARK_PALETTE : LIGHT_PALETTE;
  }

  /** 输入框底：高不透明度 + blur，保证背景内容不会透过输入文字 */
  private inputSolidBg(): string {
    return this.isDark ? 'rgba(28, 28, 30, 0.94)' : 'rgba(245, 245, 247, 0.92)';
  }

  /**
   * 由【文本本身】判断输入框是否需要换行，而不是回读控件高度。
   *
   * 用 MeasureUtils 在单行态可用宽度下测量文字：宽度超了就是多行。
   * 测量宽度恒定取单行态（窄）宽度，与控件当前实际宽度无关，
   * 所以"多行时变宽"不会反过来改变判定结果 —— 没有反馈环，也就不抖。
   */
  private recomputeMultiLine(text: string): void {
    const avail: number = this.inputRowWidth - INPUT_LEFT_GAP - INPUT_RIGHT_GAP
      - INPUT_INNER_PAD * 2;
    if (avail <= 0) {
      return;
    }
    let multi: boolean = text.indexOf('\n') >= 0;
    if (!multi && text.length > 0) {
      const opt: MeasureOptions = {
        textContent: text,
        fontSize: INPUT_FONT_SIZE,
      };
      const size: SizeOptions = this.getUIContext().getMeasureUtils().measureTextSize(opt);
      // measureTextSize 返回 px，可用宽度是 vp，换算后再比
      const widthVp: number = this.getUIContext().px2vp(size.width as number);
      multi = widthVp > avail;
    }
    if (multi !== this.inputMultiLine) {
      this.getUIContext().animateTo({ duration: 260, curve: Curve.Friction }, () => {
        this.inputMultiLine = multi;
      });
    }
  }

  // ===================== 附件：选择与上传 =====================

  /** 从图库挑一张图 */
  private async pickImage(): Promise<void> {
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.attachMenuOpen = false;
    });
    try {
      const options = new picker.PhotoSelectOptions();
      options.MIMEType = picker.PhotoViewMIMETypes.IMAGE_TYPE;
      options.maxSelectNumber = 1;
      const photoPicker = new picker.PhotoViewPicker();
      const result = await photoPicker.select(options);
      if (result.photoUris.length === 0) {
        return;
      }
      this.stagePickedFile(result.photoUris[0], true);
    } catch (e) {
      chatStore.setStage(userMessage('chat.pickImage', e));
      chatStore.forceRefresh();
    }
  }

  /** 从文件管理器挑一个文件 */
  private async pickFile(): Promise<void> {
    this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
      this.attachMenuOpen = false;
    });
    try {
      const options = new picker.DocumentSelectOptions();
      options.maxSelectNumber = 1;
      const docPicker = new picker.DocumentViewPicker();
      const uris: string[] = await docPicker.select(options);
      if (uris.length === 0) {
        return;
      }
      this.stagePickedFile(uris[0], false);
    } catch (e) {
      chatStore.setStage(userMessage('chat.pickFile', e));
      chatStore.forceRefresh();
    }
  }

  /**
   * 把 picker 给的 URI 复制到应用沙箱。
   * http 的 multiFormDataList.filePath 只能读应用自己的沙箱路径，
   * 直接把 picker 的 media:// URI 交过去会读不到内容。
   */
  private stagePickedFile(srcUri: string, isImage: boolean): void {
    try {
      const ctx = getContext(this) as common.UIAbilityContext;
      const name: string = fileNameOf(srcUri);
      const destPath: string = ctx.filesDir + '/up_' + Date.now().toString(36) + '_' + name;
      const srcFile = fileIo.openSync(srcUri, fileIo.OpenMode.READ_ONLY);
      const destFile = fileIo.openSync(destPath,
        fileIo.OpenMode.READ_WRITE | fileIo.OpenMode.CREATE | fileIo.OpenMode.TRUNC);
      fileIo.copyFileSync(srcFile.fd, destFile.fd);
      fileIo.closeSync(srcFile);
      fileIo.closeSync(destFile);
      const stat = fileIo.statSync(destPath);
      this.clearPendingFile();
      this.pendingPath = destPath;
      this.pendingName = name;
      this.pendingSize = stat.size;
      this.pendingIsImage = isImage;
      this.pendingMime = mimeOf(name, isImage);
    } catch (e) {
      chatStore.setStage(userMessage('chat.stageFile', e));
    }
    chatStore.forceRefresh();
  }

  /** 丢弃待发送附件，并删掉沙箱里的临时副本 */
  private clearPendingFile(): void {
    if (this.pendingPath.length > 0) {
      try {
        fileIo.unlinkSync(this.pendingPath);
      } catch (e) {
        // 已不存在，忽略
      }
    }
    this.pendingPath = '';
    this.pendingName = '';
    this.pendingSize = 0;
    this.pendingIsImage = false;
    this.pendingMime = '';
  }

  /** 发送：有附件走 multipart（POST /chat/file），否则走 POST /chat */
  private async send(): Promise<void> {
    if (this.loading || this.uploading) {
      return;
    }
    // 没连后端时直接返回、且不清输入/不动附件：与拆分前 sendChat 的守卫顺序一致
    if (connStore.getCurrentConnection() === null) {
      return;
    }
    const text: string = this.inputText.trim();
    if (this.pendingPath.length > 0) {
      const path: string = this.pendingPath;
      const name: string = this.pendingName;
      const size: number = this.pendingSize;
      const isImage: boolean = this.pendingIsImage;
      const mime: string = this.pendingMime;
      this.inputText = '';
      this.inputMultiLine = false;
      this.uploading = true;
      await sendChatFile(text, path, name, size, isImage, mime);
      this.uploading = false;
      this.clearPendingFile();
      chatStore.forceRefresh();
      chatStore.requestScroll();
      return;
    }
    if (text.length === 0) {
      return;
    }
    this.inputText = '';
    this.inputMultiLine = false;
    await sendChatText(text);
  }

  // ===================== UI =====================

  build() {
    NavFloatOverlay({ tab: 0 }) {
      // 加号展开的两个选项（图片 / 文件），点一次收起
      if (this.attachMenuOpen) {
        ChatAttachMenu({
          onPickImage: () => {
            this.pickImage();
          },
          onPickFile: () => {
            this.pickFile();
          },
        })
      }

      // 待发送附件预览（选好图片/文件、还没点发送时显示）
      if (this.pendingName.length > 0) {
        ChatPendingChip({
          name: this.pendingName,
          byteSize: this.pendingSize,
          isImage: this.pendingIsImage,
          uploading: this.uploading,
          onRemove: () => {
            this.clearPendingFile();
            chatStore.forceRefresh();
          },
        })
      }

      // Stack 而不是 Column：加号与发送按钮钉死在底部这一行不动，
      // 输入框是浮在它们上面的独立层，超过一行就往上长并展开到整行宽度。
      Stack({ alignContent: Alignment.Bottom }) {
        // 底层：固定不动的一行 —— 左加号（图片/文件）、右发送/中断按钮
        Row() {
          FloatIconButton({
            icon: $r('app.media.ic_plus'),
            onTap: () => {
              // 菜单展开会同时改变列表底部留白，用 animateTo 把
              // 列表内边距和菜单进出场拉到同一个时钟上。
              this.getUIContext().animateTo({ duration: ANIM_NORMAL, curve: Curve.EaseOut }, () => {
                this.attachMenuOpen = !this.attachMenuOpen;
              });
            },
          })
          Blank()
          if (this.loading) {
            Button() {
              Image($r('app.media.ic_stop'))
                .width(16)
                .height(16)
                .fillColor(Color.White)
            }
              .width(44)
              .height(44)
              .type(ButtonType.Circle)
              .backgroundColor('#77809A')
              // 发送/中断切换是 if 分支整体替换，用 transition 淡入淡出
              .transition(TransitionEffect.OPACITY.combine(TransitionEffect.scale({ x: 0.9, y: 0.9 })).animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
              .onClick(() => {
                interruptChat();
              })
          } else {
            Button() {
              Image($r('app.media.ic_send'))
                .width(18)
                .height(18)
                .fillColor(Color.White)
            }
              .width(44)
              .height(44)
              .type(ButtonType.Circle)
              .backgroundColor(this.palette().accent)
              .enabled(this.inputText.trim().length > 0 || this.pendingPath.length > 0)
              .transition(TransitionEffect.OPACITY.combine(TransitionEffect.scale({ x: 0.9, y: 0.9 })).animation({ duration: ANIM_FAST, curve: Curve.EaseOut }))
              .onClick(() => {
                this.send();
              })
          }
        }
        .width('100%')
        .height(44)
        .alignItems(VerticalAlign.Center)
        .onAreaChange((_o: Area, n: Area) => {
          // 这一行高度恒为 44、宽度恒为 100%，测量它不会形成反馈环。
          const w: number = n.width as number;
          if (Math.abs(w - this.inputRowWidth) > 0.5) {
            this.inputRowWidth = w;
            this.recomputeMultiLine(this.inputText);
          }
        })

        // 上层：输入框。
        // 单行时左右让出加号(42+8)与发送键(44+8)的位置，与它们同处一行；
        // 多行时整体上移 52 抬到那一行之上，并铺满整行宽度。
        //
        // 之前"只上移不变宽"是因为宽度被钉死了：让宽度跟着实测高度变会形成
        // 布局反馈环（变宽→文字回落成一行→变窄→又折行），卡在半弹出态抖动。
        // 现在改用 MeasureUtils 直接量文字：始终按【窄宽度】测量是否需要换行，
        // 判定输入只依赖文本内容，与控件实际宽度无关，所以变宽也不会自激。
        Row() {
          TextArea({
            placeholder: '输入消息...',
            text: this.inputText,
          })
            .layoutWeight(1)
            // 不写死高度：单行 44，随文字换行自动增高，最多约 5 行后内部滚动
            .constraintSize({ minHeight: 44, maxHeight: 168 })
            .fontSize(INPUT_FONT_SIZE)
            .fontColor(this.palette().textPrimary)
            .placeholderFont({ size: 13 })
            .placeholderColor(this.palette().textMuted)
            .backgroundColor(this.inputSolidBg())
            .backdropBlur(24)
            .borderRadius(22)
            .border({ width: 1, color: this.palette().glassBorder })
            .padding({
              left: INPUT_INNER_PAD,
              right: INPUT_INNER_PAD,
              top: 11,
              bottom: 11,
            })
            .enterKeyType(EnterKeyType.Send)
            .onChange((value: string) => {
              this.inputText = value;
              this.recomputeMultiLine(value);
            })
            .onSubmit(() => {
              this.send();
            })
        }
        .width('100%')
        // 多行时必须显式写 0：给 .padding() 传 undefined 在增量更新时会被当作
        // "不修改该属性"，旧的左右 50/52 留在原地 —— 这就是"只上移不变宽"。
        .padding(this.inputMultiLine
          ? { left: 0, right: 0 }
          : { left: INPUT_LEFT_GAP, right: INPUT_RIGHT_GAP })
        .margin({ bottom: this.inputMultiLine ? 52 : 0 })
        // 关键：这层 Row 铺满整宽，它的左右 padding 正好压在加号与发送键上方。
        // 不设 None 的话 padding 区域仍属于 Row，会把点击吞掉 —— 发送键点不动。
        // None = 自身不响应、子节点（TextArea）照常响应，触摸落到下层那一行。
        .hitTestBehavior(HitTestMode.None)
        .animation({ duration: 260, curve: Curve.Friction })
      }
      .width('100%')
    }
  }
}
