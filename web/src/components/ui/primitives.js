// primitives.js — shadcn 风格基础件(函数式,零依赖):Button/Card 家族/Badge/Skeleton/Separator/Label/Input/Spinner。
import { defineComponent, h } from 'vue';

const cx = (...xs) => xs.filter(Boolean).join(' ');

function make(tag, baseCls, name) {
  return defineComponent({
    name,
    inheritAttrs: false,
    setup(_, { attrs, slots }) {
      return () => h(tag, { ...attrs, class: cx(baseCls, attrs.class) }, slots.default?.());
    },
  });
}

export const Card = make('div', 'rounded-lg border border-border bg-card text-card-foreground shadow-sm', 'UiCard');
export const CardHeader = make('div', 'flex flex-col space-y-1.5 p-5', 'UiCardHeader');
export const CardTitle = make('h3', 'text-base font-semibold leading-none tracking-tight', 'UiCardTitle');
export const CardDescription = make('p', 'text-sm text-muted-foreground', 'UiCardDescription');
export const CardContent = make('div', 'p-5 pt-0', 'UiCardContent');
export const CardFooter = make('div', 'flex items-center p-5 pt-0', 'UiCardFooter');
export const Separator = make('div', 'shrink-0 bg-border h-px w-full', 'UiSeparator');
export const Label = make('label', 'text-sm font-medium leading-none text-foreground', 'UiLabel');
export const Skeleton = make('div', 'animate-pulse rounded-md bg-muted', 'UiSkeleton');

const BUTTON_VARIANTS = {
  default: 'bg-primary text-primary-foreground shadow hover:bg-primary/90',
  secondary: 'bg-secondary text-secondary-foreground hover:bg-secondary/80',
  outline: 'border border-input bg-card shadow-sm hover:bg-accent hover:text-accent-foreground',
  ghost: 'hover:bg-accent hover:text-accent-foreground',
  destructive: 'bg-destructive text-destructive-foreground shadow-sm hover:bg-destructive/90',
  success: 'bg-success text-success-foreground shadow-sm hover:bg-success/90',
};
const BUTTON_SIZES = {
  default: 'h-9 px-4 py-2',
  sm: 'h-8 rounded-md px-3 text-xs',
  xs: 'h-7 rounded-md px-2 text-xs',
  icon: 'h-9 w-9',
};

export const Button = defineComponent({
  name: 'UiButton',
  inheritAttrs: false,
  props: {
    variant: { type: String, default: 'default' },
    size: { type: String, default: 'default' },
    loading: { type: Boolean, default: false },
    disabled: { type: Boolean, default: false },
  },
  setup(props, { attrs, slots }) {
    return () =>
      h(
        'button',
        {
          ...attrs,
          disabled: props.disabled || props.loading || attrs.disabled,
          class: cx(
            'inline-flex items-center justify-center gap-1.5 whitespace-nowrap rounded-md text-sm font-medium',
            'transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring',
            'disabled:pointer-events-none disabled:opacity-50 [&_svg]:size-4 [&_svg]:shrink-0',
            BUTTON_VARIANTS[props.variant] ?? BUTTON_VARIANTS.default,
            BUTTON_SIZES[props.size] ?? BUTTON_SIZES.default,
            attrs.class,
          ),
        },
        [
          props.loading ? h(Spinner, { class: 'animate-spin' }) : null,
          slots.default?.(),
        ],
      );
  },
});

export const Spinner = defineComponent({
  name: 'UiSpinner',
  inheritAttrs: false,
  setup(_, { attrs }) {
    return () =>
      h(
        'svg',
        { ...attrs, class: attrs.class, viewBox: '0 0 24 24', fill: 'none', xmlns: 'http://www.w3.org/2000/svg' },
        [
          h('circle', { cx: '12', cy: '12', r: '10', stroke: 'currentColor', 'stroke-width': '4', class: 'opacity-25' }),
          h('path', { d: 'M22 12a10 10 0 0 0-10-10', stroke: 'currentColor', 'stroke-width': '4', 'stroke-linecap': 'round' }),
        ],
      );
  },
});

const BADGE_VARIANTS = {
  default: 'border-transparent bg-primary text-primary-foreground',
  secondary: 'border-transparent bg-secondary text-secondary-foreground',
  destructive: 'border-transparent bg-destructive text-destructive-foreground',
  success: 'border-transparent bg-success text-success-foreground',
  warning: 'border-transparent bg-warning text-warning-foreground',
  outline: 'text-foreground',
};

export const Badge = defineComponent({
  name: 'UiBadge',
  inheritAttrs: false,
  props: { variant: { type: String, default: 'default' } },
  setup(props, { attrs, slots }) {
    return () =>
      h(
        'span',
        {
          ...attrs,
          class: cx(
            'inline-flex items-center rounded-md border px-2 py-0.5 text-xs font-medium transition-colors',
            'focus:outline-none [&_svg]:size-3 [&_svg]:shrink-0 gap-1 w-fit',
            BADGE_VARIANTS[props.variant] ?? BADGE_VARIANTS.default,
            attrs.class,
          ),
        },
        slots.default?.(),
      );
  },
});

export const Input = defineComponent({
  name: 'UiInput',
  inheritAttrs: false,
  props: { modelValue: { type: [String, Number], default: '' } },
  emits: ['update:modelValue'],
  setup(props, { attrs, emit }) {
    return () =>
      h('input', {
        ...attrs,
        value: props.modelValue,
        onInput: (e) => emit('update:modelValue', e.target.value),
        class: cx(
          'flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors',
          'placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring',
          'disabled:cursor-not-allowed disabled:opacity-50',
          attrs.class,
        ),
      });
  },
});
