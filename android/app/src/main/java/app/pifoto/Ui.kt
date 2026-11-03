package app.pifoto

import android.animation.ObjectAnimator
import android.animation.PropertyValuesHolder
import android.animation.StateListAnimator
import android.animation.TimeInterpolator
import android.animation.ValueAnimator
import android.app.Activity
import android.content.Context
import android.content.res.ColorStateList
import android.graphics.Canvas
import android.graphics.Paint
import android.graphics.Path
import android.graphics.PathMeasure
import android.graphics.RectF
import android.graphics.Typeface
import android.graphics.drawable.Drawable
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.RippleDrawable
import android.os.Build
import android.os.SystemClock
import android.text.TextUtils
import android.view.Gravity
import android.view.View
import android.view.accessibility.AccessibilityNodeInfo
import android.view.animation.PathInterpolator
import android.widget.Button
import android.widget.FrameLayout
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.Switch
import android.widget.TextView
import android.window.OnBackInvokedCallback
import android.window.OnBackInvokedDispatcher
import kotlin.math.cos
import kotlin.math.exp
import kotlin.math.min
import kotlin.math.sin
import kotlin.math.sqrt

/** Colour tokens from the PiFoto system board. */
object Pal {
    const val BG = 0xFF111214.toInt()
    const val SURFACE = 0xFF1B1C20.toInt()     // cards, groups
    const val RAISED = 0xFF1F2025.toInt()      // sheets, dialogs
    const val FIELD = 0xFF1C1D21.toInt()
    const val FG = 0xFFECECEE.toInt()
    const val MUT = 0xFFA0A2A9.toInt()
    const val ACC = 0xFF6AA8FF.toInt()
    const val ON_ACC = 0xFF071223.toInt()
    const val WARN = 0xFFFFB35C.toInt()
    const val BAD = 0xFFFF6B6B.toInt()
    const val ON_BAD = 0xFF2A0707.toInt()
    const val BAD_TEXT = 0xFFFF8A82.toInt()
    const val ERR_BORDER = 0xFFFF7A72.toInt()
    const val TRACK = 0xFF3A3B42.toInt()       // switch off, outline buttons
    const val THUMB = 0xFFE8E9EC.toInt()
    const val LINE = 0x0FFFFFFF                // rgba(255,255,255,.06): dividers
    const val FIELD_LINE = 0x14FFFFFF          // .08
    const val TONAL = 0x14FFFFFF               // .08: secondary buttons
    const val PRESS = 0x1AFFFFFF               // .10: pressed ghost buttons
    const val BAR_TRACK = 0x1AFFFFFF           // .10
    const val ACC_TINT = 0x246AA8FF            // .14
    const val WARN_TINT = 0x24FFB35C
    const val OFF_TINT = 0x0FFFFFFF
    const val SCRIM = 0x8C000000.toInt()       // .55
    const val HINT = 0x66ECECEE
}

/** Motion tokens: transform and opacity only. */
object Motion {
    /** cubic-bezier(.2,0,0,1): fades, colour changes, presses. */
    val EO: TimeInterpolator = PathInterpolator(0.2f, 0f, 0f, 1f)

    /** --sp: a critically damped spring (no overshoot). */
    val SPRING = TimeInterpolator { t ->
        if (t >= 1f) 1f else { val a = 9.3f * t; (1f - (1f + a) * exp(-a)) / (1f - 10.3f * exp(-9.3f)) }
    }

    /** --sb: a spring with ~1.5% overshoot, only for things that follow a gesture (switch thumb). */
    val BOUNCE = TimeInterpolator { t ->
        if (t >= 1f) 1f else {
            val z = 0.8; val w = 8.73; val wd = w * sqrt(1 - z * z); val tt = t.toDouble()
            (1 - exp(-z * w * tt) * (cos(wd * tt) + z * w / wd * sin(wd * tt))).toFloat()
        }
    }
}

fun Context.dp(v: Float): Int = (v * resources.displayMetrics.density + 0.5f).toInt()
fun Context.dp(v: Int): Int = dp(v.toFloat())
fun Context.dpf(v: Float): Float = v * resources.displayMetrics.density

/** system-ui at a given weight (variable system fonts render 650 exactly; others snap to the nearest). */
fun font(weight: Int): Typeface = Typeface.create(Typeface.SANS_SERIF, weight, false)

fun Context.label(text: CharSequence = "", size: Float = 15f, weight: Int = 400, color: Int = Pal.FG,
                  spacing: Float = 0f, line: Float = 1.4f) = TextView(this).apply {
    this.text = text
    textSize = size
    typeface = font(weight)
    setTextColor(color)
    letterSpacing = spacing
    includeFontPadding = false
    setLineHeight((textSize * line).toInt()) // textSize is px here, so this follows the user's font scale
}

fun rounded(color: Int, radius: Float, strokePx: Int = 0, strokeColor: Int = 0) = GradientDrawable().apply {
    setColor(color); cornerRadius = radius
    if (strokePx > 0) setStroke(strokePx, strokeColor)
}

/** Pressed feedback: a soft white wash clipped to [shape] (or the content). */
fun ripple(content: Drawable?, shape: Drawable? = content, color: Int = Pal.PRESS) =
    RippleDrawable(ColorStateList.valueOf(color), content, shape)

/** Buttons and tiles scale to [s] while pressed (140 ms, on pointer-down). */
fun View.pressScale(s: Float = 0.97f) {
    fun anim(v: Float) = ObjectAnimator.ofPropertyValuesHolder(this,
        PropertyValuesHolder.ofFloat(View.SCALE_X, v), PropertyValuesHolder.ofFloat(View.SCALE_Y, v))
        .apply { duration = 140; interpolator = Motion.EO }
    stateListAnimator = StateListAnimator().apply {
        addState(intArrayOf(android.R.attr.state_pressed), anim(s))
        addState(IntArray(0), anim(1f))
    }
}

fun Context.icon(res: Int, sizeDp: Float, color: Int) = ImageView(this).apply {
    setImageResource(res)
    imageTintList = ColorStateList.valueOf(color)
    scaleType = ImageView.ScaleType.FIT_CENTER
    layoutParams = LinearLayout.LayoutParams(dp(sizeDp), dp(sizeDp))
    importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
}

/** A tinted circle with an icon: status badges and sheet headers. */
fun Context.badge(res: Int, sizeDp: Float, iconDp: Float, tint: Int, color: Int) = FrameLayout(this).apply {
    background = rounded(tint, dpf(sizeDp / 2))
    addView(icon(res, iconDp, color), FrameLayout.LayoutParams(dp(iconDp), dp(iconDp), Gravity.CENTER))
    layoutParams = LinearLayout.LayoutParams(dp(sizeDp), dp(sizeDp))
}

fun Context.divider(insetDp: Float = 20f) = View(this).apply {
    setBackgroundColor(Pal.LINE)
    layoutParams = LinearLayout.LayoutParams(-1, dp(1)).apply { marginStart = dp(insetDp); marginEnd = dp(insetDp) }
}

/** Vertical LinearLayout that never grows wider than [maxWidthPx] (centre it with layout gravity). */
open class Column(ctx: Context, var maxWidthPx: Int = Int.MAX_VALUE) : LinearLayout(ctx) {
    init { orientation = VERTICAL }
    override fun onMeasure(widthSpec: Int, heightSpec: Int) {
        val w = MeasureSpec.getSize(widthSpec)
        super.onMeasure(if (w > maxWidthPx) MeasureSpec.makeMeasureSpec(maxWidthPx, MeasureSpec.EXACTLY) else widthSpec, heightSpec)
    }
}

/** Adds [v] with a top gap, so columns read like the design's flex gaps. */
fun LinearLayout.add(v: View, gapDp: Float = 0f, width: Int = -1, height: Int = -2, gravity: Int = -1): View {
    val lp = LinearLayout.LayoutParams(width, height)
    if (gapDp > 0) lp.topMargin = context.dp(gapDp)
    if (gravity != -1) lp.gravity = gravity
    addView(v, lp)
    return v
}

/** Indeterminate ring (track + quarter arc), 0.9 s per turn. */
class Spinner(ctx: Context, private val track: Int, private val arc: Int, private val strokeOf24: Float = 2.6f) : View(ctx) {
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG).apply { style = Paint.Style.STROKE; strokeCap = Paint.Cap.ROUND }
    private val box = RectF()
    init { importantForAccessibility = IMPORTANT_FOR_ACCESSIBILITY_NO }
    override fun onDraw(canvas: Canvas) {
        val s = min(width, height).toFloat()
        val r = s * 9f / 24f
        paint.strokeWidth = s * strokeOf24 / 24f
        box.set(width / 2f - r, height / 2f - r, width / 2f + r, height / 2f + r)
        paint.color = track; canvas.drawOval(box, paint)
        paint.color = arc
        canvas.drawArc(box, -90f + (SystemClock.uptimeMillis() % 900) / 900f * 360f, 90f, false, paint)
        if (isShown && alpha > 0f) postInvalidateOnAnimation()
    }
}

/** Pill button (52 dp by default) in the design's styles; can swap its label for a spinner. */
class Pill(ctx: Context, text: String, private val style: Style, iconRes: Int = 0, heightDp: Float = 52f,
           weight: Int = if (style == Style.PRIMARY || style == Style.DANGER) 650 else 600, textSize: Float = 16f) : FrameLayout(ctx) {
    enum class Style { PRIMARY, TONAL, GHOST, OUTLINE, DANGER }

    val text: TextView
    private val content = LinearLayout(ctx).apply { orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL }
    private var spinner: Spinner? = null
    var busy = false; private set

    init {
        val fg = when (style) { Style.PRIMARY -> Pal.ON_ACC; Style.DANGER -> Pal.ON_BAD; else -> Pal.FG }
        val fill = when (style) {
            Style.PRIMARY -> rounded(Pal.ACC, 999f)
            Style.DANGER -> rounded(Pal.BAD, 999f)
            Style.TONAL -> rounded(Pal.TONAL, 999f)
            Style.OUTLINE -> rounded(0, 999f, ctx.dp(1), Pal.TRACK)
            Style.GHOST -> null
        }
        background = ripple(fill, rounded(-1, 999f), if (style == Style.PRIMARY || style == Style.DANGER) 0x1F000000 else Pal.PRESS)
        if (iconRes != 0) content.addView(ctx.icon(iconRes, 20f, fg).apply {
            (layoutParams as LinearLayout.LayoutParams).marginEnd = ctx.dp(8)
        })
        this.text = ctx.label(text, textSize, weight, fg, line = 1.2f).apply { maxLines = 1; ellipsize = TextUtils.TruncateAt.END }
        content.addView(this.text)
        addView(content, LayoutParams(-2, -2, Gravity.CENTER))
        minimumHeight = ctx.dp(heightDp)
        setPadding(ctx.dp(24), 0, ctx.dp(24), 0)
        isClickable = true; isFocusable = true
        pressScale()
    }

    fun setColor(c: Int) { text.setTextColor(c) }

    /** Label out, spinner in (and the button stops taking taps), like the design's Log in / Retry. */
    fun setBusy(b: Boolean) {
        if (b == busy) return
        busy = b
        val sp = spinner ?: Spinner(context,
            if (style == Style.PRIMARY) 0x33071223 else 0x1FFFFFFF,
            if (style == Style.PRIMARY) Pal.ON_ACC else Pal.ACC).also {
            spinner = it; it.alpha = 0f
            addView(it, LayoutParams(context.dp(22), context.dp(22), Gravity.CENTER))
        }
        isClickable = !b
        content.animate().alpha(if (b) 0f else 1f).scaleX(if (b) .9f else 1f).scaleY(if (b) .9f else 1f)
            .setDuration(if (b) 160 else 300).setInterpolator(if (b) Motion.EO else Motion.SPRING).start()
        sp.visibility = VISIBLE
        sp.animate().alpha(if (b) 1f else 0f).setDuration(160).setInterpolator(Motion.EO)
            .withEndAction { if (!busy) sp.visibility = INVISIBLE }.start()
        stateDescription = if (b) "Working" else null
    }

    override fun setEnabled(enabled: Boolean) {
        super.setEnabled(enabled)
        alpha = if (enabled) 1f else 0.35f
    }

    override fun getAccessibilityClassName(): CharSequence = Button::class.java.name
}

/** The design's switch: 52×32 track, 24 dp thumb that stretches while pressed and springs across. */
class PiSwitch(ctx: Context) : View(ctx) {
    var checked = false; private set
    private var pos = 0f       // 0 = off, 1 = on
    private var press = 0f     // 0..1 thumb stretch
    private var posAnim: ValueAnimator? = null
    private var pressAnim: ValueAnimator? = null
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    private val shadow = Paint(Paint.ANTI_ALIAS_FLAG).apply { color = 0; setShadowLayer(ctx.dpf(3f), 0f, ctx.dpf(1f), 0x59000000) }
    private val r = RectF()

    init { isDuplicateParentStateEnabled = true; importantForAccessibility = IMPORTANT_FOR_ACCESSIBILITY_NO }

    override fun onMeasure(widthMeasureSpec: Int, heightMeasureSpec: Int) =
        setMeasuredDimension(context.dp(52), context.dp(32))

    fun setChecked(on: Boolean, animate: Boolean) {
        if (on == checked && posAnim == null) { pos = if (on) 1f else 0f; invalidate(); return }
        checked = on
        posAnim?.cancel()
        if (!animate) { pos = if (on) 1f else 0f; posAnim = null; invalidate(); return }
        posAnim = ValueAnimator.ofFloat(pos, if (on) 1f else 0f).apply {
            duration = 460; interpolator = Motion.BOUNCE
            addUpdateListener { pos = it.animatedValue as Float; invalidate() }
            start()
        }
    }

    override fun drawableStateChanged() {
        super.drawableStateChanged()
        val target = if (isPressed) 1f else 0f
        if (target == press) return
        pressAnim?.cancel()
        pressAnim = ValueAnimator.ofFloat(press, target).apply {
            duration = 200; interpolator = Motion.EO
            addUpdateListener { press = it.animatedValue as Float; invalidate() }
            start()
        }
    }

    private fun mix(a: Int, b: Int, t: Float): Int {
        val tt = t.coerceIn(0f, 1f)
        fun ch(s: Int) = (((a shr s) and 0xFF) + ((((b shr s) and 0xFF) - ((a shr s) and 0xFF)) * tt)).toInt() shl s
        return ch(24) or ch(16) or ch(8) or ch(0)
    }

    override fun onDraw(canvas: Canvas) {
        val d = context.resources.displayMetrics.density
        val h = height.toFloat()
        r.set(0f, 0f, width.toFloat(), h)
        paint.color = mix(Pal.TRACK, Pal.ACC, pos)
        canvas.drawRoundRect(r, h / 2, h / 2, paint)
        val tw = (24f + 4f * press) * d                    // thumb width (stretches while pressed)
        val left = 4f * d + pos * (width - 8f * d - tw)    // slides between the 4 dp insets
        r.set(left, 4f * d, left + tw, 28f * d)
        canvas.drawRoundRect(r, 12f * d, 12f * d, shadow)
        paint.color = mix(Pal.THUMB, -1, pos)
        canvas.drawRoundRect(r, 12f * d, 12f * d, paint)
    }
}

/** A settings row that toggles a [PiSwitch]; announced to TalkBack as a switch. */
class SwitchRow(ctx: Context, title: String, subtitle: String? = null, private val onToggle: (Boolean) -> Unit) : LinearLayout(ctx) {
    val switch = PiSwitch(ctx)
    val checked get() = switch.checked

    init {
        orientation = HORIZONTAL
        gravity = Gravity.CENTER_VERTICAL
        setPadding(ctx.dp(20), ctx.dp(16), ctx.dp(20), ctx.dp(16))
        minimumHeight = ctx.dp(if (subtitle != null) 72 else 64)
        background = ripple(null, rounded(-1, 0f), 0x0AFFFFFF)
        val texts = LinearLayout(ctx).apply { orientation = VERTICAL }
        texts.addView(ctx.label(title, 16f, line = 1.3f))
        if (subtitle != null) texts.add(ctx.label(subtitle, 13f, color = Pal.MUT, line = 1.3f), 2f)
        addView(texts, LayoutParams(0, -2, 1f).apply { marginEnd = ctx.dp(16) })
        addView(switch)
        isClickable = true; isFocusable = true
        setOnClickListener { val on = !switch.checked; switch.setChecked(on, true); onToggle(on) }
    }

    fun setChecked(on: Boolean) { if (on != switch.checked) switch.setChecked(on, isLaidOut) }

    override fun setEnabled(enabled: Boolean) {
        super.setEnabled(enabled)
        animate().alpha(if (enabled) 1f else 0.38f).setDuration(220).setInterpolator(Motion.EO).start()
    }

    override fun getAccessibilityClassName(): CharSequence = Switch::class.java.name
    override fun onInitializeAccessibilityNodeInfo(info: AccessibilityNodeInfo) {
        super.onInitializeAccessibilityNodeInfo(info)
        info.isCheckable = true
        @Suppress("DEPRECATION") // the tri-state replacement is API 36+; this works everywhere
        info.isChecked = switch.checked
    }
}

/** 6 dp progress line; eases to new values so 0.5 s updates read as continuous movement. */
class ProgressLine(ctx: Context) : View(ctx) {
    private var frac = 0f
    private var anim: ValueAnimator? = null
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG)
    private val r = RectF()
    private val clip = Path()

    fun set(f: Float, animate: Boolean) {
        val target = f.coerceIn(0f, 1f)
        anim?.cancel()
        if (!animate || !isLaidOut) { frac = target; invalidate(); return }
        anim = ValueAnimator.ofFloat(frac, target).apply {
            duration = 450; interpolator = null
            addUpdateListener { frac = it.animatedValue as Float; invalidate() }
            start()
        }
    }

    override fun onDraw(canvas: Canvas) {
        val h = height.toFloat()
        r.set(0f, 0f, width.toFloat(), h)
        paint.color = Pal.BAR_TRACK
        canvas.drawRoundRect(r, h / 2, h / 2, paint)
        clip.reset(); clip.addRoundRect(r, h / 2, h / 2, Path.Direction.CW)
        canvas.save(); canvas.clipPath(clip)
        r.right = width * frac
        paint.color = Pal.ACC
        canvas.drawRoundRect(r, h / 2, h / 2, paint)
        canvas.restore()
    }
}

/** The "You're all set" tick that draws itself in. */
class DrawnCheck(ctx: Context, private val color: Int) : View(ctx) {
    private val path = Path()
    private val seg = Path()
    private val measure = PathMeasure()
    private var t = 0f
    private val paint = Paint(Paint.ANTI_ALIAS_FLAG).apply {
        style = Paint.Style.STROKE; strokeCap = Paint.Cap.ROUND; strokeJoin = Paint.Join.ROUND
    }

    fun play(delayMs: Long) {
        ValueAnimator.ofFloat(0f, 1f).apply {
            duration = 520; startDelay = delayMs; interpolator = Motion.EO
            addUpdateListener { t = it.animatedValue as Float; invalidate() }
            start()
        }
    }

    override fun onSizeChanged(w: Int, h: Int, oldw: Int, oldh: Int) {
        val k = w / 24f
        path.reset(); path.moveTo(6f * k, 12.5f * k); path.lineTo(10f * k, 16.5f * k); path.lineTo(18f * k, 7.5f * k)
        measure.setPath(path, false)
        paint.strokeWidth = 2.4f * k
    }

    override fun onDraw(canvas: Canvas) {
        seg.reset()
        measure.getSegment(0f, measure.length * t, seg, true)
        paint.color = color
        canvas.drawPath(seg, paint)
    }
}

/** Fade + 14 dp rise with the spring (the design's .rise / .rise2 / .rise3). */
fun View.rise(delayMs: Long = 0, distDp: Float = 14f, durationMs: Long = 620) {
    alpha = 0f; translationY = context.dpf(distDp)
    animate().alpha(1f).translationY(0f).setStartDelay(delayMs).setDuration(durationMs).setInterpolator(Motion.SPRING).start()
}

/** Small horizontal shake for a rejected field. */
fun View.shake() {
    val d = context.dpf(1f)
    ObjectAnimator.ofFloat(this, View.TRANSLATION_X, 0f, -7 * d, 6 * d, -4 * d, 2 * d, 0f).apply {
        duration = 380; interpolator = Motion.EO; start()
    }
}

/**
 * Bottom sheets (the design's .bs): a scrim plus a raised card that springs up from the bottom.
 * Lives on top of the activity's root FrameLayout; handles Back while open.
 */
class Sheets(private val act: Activity, private val root: FrameLayout) {
    private val scrim = View(act).apply {
        setBackgroundColor(Pal.SCRIM); alpha = 0f; visibility = View.GONE
        isClickable = true
        setOnClickListener { if (dismissable) cancel() }
        importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
    }
    private val card = Column(act, act.dp(480)).apply {
        background = rounded(Pal.RAISED, act.dpf(28f))
        setPadding(act.dp(20), act.dp(28), act.dp(20), act.dp(20))
        elevation = act.dpf(24f)
        visibility = View.GONE
        isClickable = true // don't let taps fall through to the scrim
    }
    var current: String? = null; private set
    private var dismissable = true
    private var onCancel: (() -> Unit)? = null
    private var backCb: Any? = null

    init {
        root.addView(scrim, FrameLayout.LayoutParams(-1, -1))
        root.addView(card, FrameLayout.LayoutParams(-1, -2, Gravity.BOTTOM or Gravity.CENTER_HORIZONTAL))
        setInsets(0, 0, 0)
    }

    /** System bars / keyboard: keep the card 8 dp clear of them. */
    fun setInsets(left: Int, right: Int, bottom: Int) {
        val lp = card.layoutParams as FrameLayout.LayoutParams
        val m = act.dp(8)
        lp.setMargins(left + m, 0, right + m, bottom + m)
        card.layoutParams = lp
    }

    /** Shows [content] (or swaps it in if a sheet is already up). [onCancel] runs on Back / scrim tap. */
    fun show(tag: String, title: String, content: View, dismissable: Boolean = true, onCancel: (() -> Unit)? = null) {
        this.dismissable = dismissable
        this.onCancel = onCancel
        val wasOpen = current != null
        current = tag
        for (i in 0 until root.childCount) root.getChildAt(i).let {
            if (it !== scrim && it !== card) it.importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO_HIDE_DESCENDANTS
        }
        if (wasOpen) {
            card.removeAllViews(); card.addView(content)
            content.alpha = 0f
            content.animate().alpha(1f).setDuration(200).setInterpolator(Motion.EO).start()
        } else {
            card.removeAllViews(); card.addView(content)
            scrim.visibility = View.VISIBLE
            scrim.animate().alpha(1f).setDuration(260).setInterpolator(Motion.EO).start()
            card.visibility = View.VISIBLE
            card.translationY = root.height.toFloat().coerceAtLeast(act.dpf(600f))
            card.post {
                card.translationY = card.height + act.dpf(24f) + (card.layoutParams as FrameLayout.LayoutParams).bottomMargin
                card.animate().translationY(0f).setDuration(520).setInterpolator(Motion.SPRING).start()
            }
        }
        card.accessibilityPaneTitle = title
        if (Build.VERSION.SDK_INT >= 33 && backCb == null) {
            val cb = OnBackInvokedCallback { onBack() }
            act.onBackInvokedDispatcher.registerOnBackInvokedCallback(OnBackInvokedDispatcher.PRIORITY_OVERLAY, cb)
            backCb = cb
        }
    }

    fun hide() {
        if (current == null) return
        current = null
        onCancel = null
        for (i in 0 until root.childCount) root.getChildAt(i).let {
            if (it !== scrim && it !== card) it.importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_AUTO
        }
        scrim.animate().alpha(0f).setDuration(200).setInterpolator(Motion.EO)
            .withEndAction { if (current == null) scrim.visibility = View.GONE }.start()
        card.animate().translationY(card.height + act.dpf(32f)).setDuration(260).setInterpolator(Motion.EO)
            .withEndAction { if (current == null) { card.visibility = View.GONE; card.removeAllViews() } }.start()
        if (Build.VERSION.SDK_INT >= 33) (backCb as? OnBackInvokedCallback)?.let {
            act.onBackInvokedDispatcher.unregisterOnBackInvokedCallback(it)
        }
        backCb = null
    }

    private fun cancel() { val c = onCancel; hide(); c?.invoke() }

    /** Back while a sheet is up: cancel it if allowed. Returns true if consumed. */
    fun onBack(): Boolean {
        if (current == null) return false
        if (dismissable) cancel()
        return true
    }

    /** Standard sheet body: optional badge, title, text, small print, then full-width buttons. */
    fun body(title: String, text: String, buttons: List<Pill>, iconRes: Int = 0, iconColor: Int = Pal.ACC,
             iconTint: Int = Pal.ACC_TINT, small: String? = null): View {
        val c = LinearLayout(act).apply { orientation = LinearLayout.VERTICAL }
        if (iconRes != 0) c.add(act.badge(iconRes, 48f, 24f, iconTint, iconColor), width = act.dp(48), height = act.dp(48))
        c.add(act.label(title, 20f, 650, spacing = -0.01f, line = 1.25f), if (iconRes != 0) 16f else 0f)
        c.add(act.label(text, 15f, color = Pal.MUT), 8f)
        if (small != null) c.add(act.label(small, 13f, color = Pal.MUT), 8f)
        buttons.forEachIndexed { i, b -> c.add(b, if (i == 0) 24f else 8f) }
        return c
    }
}

/** Lays a view out like CSS `justify-content: space-between` for two texts. */
fun Context.spread(a: View, b: View) = LinearLayout(this).apply {
    orientation = LinearLayout.HORIZONTAL
    addView(a, LinearLayout.LayoutParams(0, -2, 1f))
    addView(b, LinearLayout.LayoutParams(-2, -2).apply { marginStart = dp(12) })
}
