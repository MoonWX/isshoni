import { act, fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { Button, buttonClass } from './Button';
import { Dialog } from './Dialog';
import { Field, TextField } from './Field';
import { Popover } from './Popover';
import { Sheet } from './Sheet';
import { PageSpinner, Spinner } from './Spinner';
import { Stepper } from './Stepper';
import { VisuallyHidden } from './VisuallyHidden';

describe('Button', () => {
  it('is type=button unless asked, and calls onClick', async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Save changes</Button>);
    const b = screen.getByRole('button', { name: 'Save changes' });
    expect(b).toHaveAttribute('type', 'button');
    await userEvent.click(b);
    expect(onClick).toHaveBeenCalledOnce();
  });

  it('while loading: busy, focusable, and clicks do nothing (no double submit)', async () => {
    const onSubmit = vi.fn((e: React.SyntheticEvent) => {
      e.preventDefault();
    });
    const onClick = vi.fn();
    render(
      <form onSubmit={onSubmit}>
        <Button type="submit" loading onClick={onClick}>
          Log in
        </Button>
      </form>,
    );
    const b = screen.getByRole('button', { name: 'Log in' });
    expect(b).toHaveAttribute('aria-busy', 'true');
    expect(b).not.toBeDisabled();
    await userEvent.click(b);
    expect(onClick).not.toHaveBeenCalled();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('gives links the same look', () => {
    expect(buttonClass({ variant: 'primary', block: true }).split(' ')).toHaveLength(4);
  });
});

describe('Field and TextField', () => {
  it('ties the label, hint and error to the control', () => {
    render(<TextField label="Username" hint="2 to 32 characters" error="That username is taken." name="username" />);
    const input = screen.getByRole('textbox', { name: 'Username' });
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(input).toHaveAccessibleDescription('2 to 32 characters That username is taken.');
  });

  it('has no aria-invalid or description without them', () => {
    render(<TextField label="Password" type="password" />);
    const input = screen.getByLabelText('Password');
    expect(input).not.toHaveAttribute('aria-invalid');
    expect(input).not.toHaveAttribute('aria-describedby');
  });

  it('wraps any control through the render prop', () => {
    render(
      <Field label="Mode">
        {(control) => (
          <select {...control}>
            <option>invite</option>
          </select>
        )}
      </Field>,
    );
    expect(screen.getByRole('combobox', { name: 'Mode' })).toBeInTheDocument();
  });
});

describe('Dialog and Sheet', () => {
  function Harness({ dismissible = true, sheet = false }: { dismissible?: boolean; sheet?: boolean }) {
    const [open, setOpen] = useState(true);
    const props = {
      open,
      onClose: () => {
        setOpen(false);
      },
      title: 'Stop sharing?',
      dismissible,
      footer: (
        <Button
          onClick={() => {
            setOpen(false);
          }}
        >
          Keep sharing
        </Button>
      ),
      children: <p>Switching rooms ends your share.</p>,
    };
    return (
      <>
        {sheet ? <Sheet side="end" {...props} /> : <Dialog {...props} />}
        <span data-testid="state">{open ? 'open' : 'closed'}</span>
      </>
    );
  }

  it('opens, is named by its title, and closes with the ✕ button', async () => {
    render(<Harness />);
    const dialog = screen.getByRole('dialog', { name: 'Stop sharing?' });
    expect(dialog).toHaveAttribute('open');
    await userEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.getByTestId('state')).toHaveTextContent('closed');
    expect(dialog).not.toHaveAttribute('open');
  });

  it('closes on Esc (cancel) only when dismissible', () => {
    const { unmount } = render(<Harness />);
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }));
    expect(screen.getByTestId('state')).toHaveTextContent('closed');
    unmount();
    render(<Harness dismissible={false} />);
    expect(screen.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();
    const cancel = new Event('cancel', { cancelable: true });
    fireEvent(screen.getByRole('dialog'), cancel);
    expect(cancel.defaultPrevented).toBe(true);
    expect(screen.getByTestId('state')).toHaveTextContent('open');
  });

  it('closes on a click on the backdrop (the <dialog> itself), not on its content', async () => {
    render(<Harness />);
    await userEvent.click(screen.getByText('Switching rooms ends your share.'));
    expect(screen.getByTestId('state')).toHaveTextContent('open');
    await userEvent.click(screen.getByRole('dialog'));
    expect(screen.getByTestId('state')).toHaveTextContent('closed');
  });

  it('uses showModal() where the browser has it', () => {
    const showModal = vi.fn(function (this: HTMLDialogElement) {
      this.setAttribute('open', '');
    });
    const close = vi.fn(function (this: HTMLDialogElement) {
      this.removeAttribute('open');
    });
    Object.assign(HTMLDialogElement.prototype, { showModal, close });
    try {
      const { unmount } = render(<Harness sheet />);
      expect(showModal).toHaveBeenCalledOnce();
      unmount();
      expect(close).toHaveBeenCalledOnce();
    } finally {
      Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal');
      Reflect.deleteProperty(HTMLDialogElement.prototype, 'close');
    }
  });
});

describe('Popover', () => {
  function Harness() {
    return (
      <>
        <Popover label="Watching now" trigger={(p) => <button {...p}>3 watching</button>}>
          {(close) => (
            <ul>
              <li>alex</li>
              <li>
                <button onClick={close}>Done</button>
              </li>
            </ul>
          )}
        </Popover>
        <button>elsewhere</button>
      </>
    );
  }

  it('opens from its trigger, takes focus, and closes on Esc with focus back on the trigger', async () => {
    render(<Harness />);
    const trigger = screen.getByRole('button', { name: '3 watching' });
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(trigger);
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    const panel = screen.getByRole('dialog', { name: 'Watching now' });
    expect(panel).toHaveFocus();
    expect(screen.getByText('alex')).toBeInTheDocument();
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByText('alex')).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });

  it('closes on a pointer press outside, on focus leaving, and through close()', async () => {
    render(<Harness />);
    const trigger = screen.getByRole('button', { name: '3 watching' });
    await userEvent.click(trigger);
    await userEvent.click(screen.getByRole('button', { name: 'elsewhere' }));
    expect(trigger).toHaveAttribute('aria-expanded', 'false');

    await userEvent.click(trigger);
    await userEvent.click(screen.getByRole('button', { name: 'Done' }));
    expect(trigger).toHaveAttribute('aria-expanded', 'false');

    await userEvent.click(trigger);
    act(() => {
      screen.getByRole('button', { name: 'elsewhere' }).focus();
    });
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
  });
});

describe('Stepper, Spinner, VisuallyHidden', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('marks the current step and says which are done', () => {
    render(<Stepper label="Setup" steps={['Admin account', 'Connection test', 'Invite friends']} current={1} />);
    const list = screen.getByRole('list', { name: 'Setup' });
    const items = screen.getAllByRole('listitem');
    expect(list).toContainElement(items[0] ?? null);
    expect(items[0]).toHaveTextContent('Step 1 of 3, done: Admin account');
    expect(items[1]).toHaveAttribute('aria-current', 'step');
    expect(items[1]).toHaveTextContent('Step 2 of 3: Connection test');
    expect(items[2]).not.toHaveAttribute('aria-current');
  });

  it('Spinner is a status with a label, or silent inside a labelled control', () => {
    const { unmount } = render(<Spinner />);
    expect(screen.getByRole('status')).toHaveTextContent('Loading');
    unmount();
    render(<Spinner label={null} />);
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
  });

  it('PageSpinner appears only after its delay', () => {
    vi.useFakeTimers();
    render(<PageSpinner />);
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(300);
    });
    expect(screen.getByRole('status')).toBeInTheDocument();
  });

  it('VisuallyHidden keeps text for screen readers', () => {
    render(
      <button>
        <VisuallyHidden>Mute alex</VisuallyHidden>
      </button>,
    );
    expect(screen.getByRole('button', { name: 'Mute alex' })).toBeInTheDocument();
  });
});
