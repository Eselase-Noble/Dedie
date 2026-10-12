package org.dedie.desktop;

import java.awt.Color;
import java.awt.Dimension;
import java.awt.EventQueue;
import java.awt.Font;
import java.awt.FontMetrics;
import java.awt.Graphics2D;
import java.awt.GraphicsEnvironment;
import java.awt.Image;
import java.awt.RenderingHints;
import java.awt.SystemTray;
import java.awt.TrayIcon;
import java.awt.event.MouseAdapter;
import java.awt.event.MouseEvent;
import java.awt.image.BufferedImage;
import java.util.function.Consumer;

/**
 * Notifications shown outside the window, through the system tray.
 *
 * <p>The tray belongs to AWT, a second interface toolkit beside JavaFX.
 * The two share a process without trouble as long as AWT is started away
 * from the JavaFX thread and each is only called on its own, so everything
 * here that touches AWT runs on a background thread or on AWT's event
 * queue, and the window is reached through the callback alone.
 *
 * <p>On Linux the desktop's own notification service is used where its
 * command line client, notify-send, is installed: what AWT draws there
 * instead is a window of its own that no desktop treats as a
 * notification, and many desktops have no tray for it to hang from.
 *
 * <p>A tray is a courtesy, not a requirement. Where there is none (many
 * Linux desktops, a session without a display) or it cannot be set up,
 * this object quietly does nothing and the interface's own notifications,
 * shown inside the window, are what the user gets.
 */
final class Notifier {

    private final Consumer<String> onOpen;
    /** Set once the icon is in the tray; null before that and where there is no tray. */
    private volatile TrayIcon icon;
    /** The page of the latest notification, opened by the next click. */
    private volatile String pendingLink;
    private volatile boolean started;
    private volatile boolean closed;
    /** The desktop's notification command on Linux, or null where there is none. */
    private final String notifySend = findNotifySend();

    /**
     * @param onOpen called when the user clicks the icon or a notification,
     *               with the page to show or null to only raise the window.
     *               It is called on AWT's thread and must hand over to
     *               JavaFX itself.
     */
    Notifier(Consumer<String> onOpen) {
        this.onOpen = onOpen;
    }

    /**
     * Puts the icon in the tray, in the background: starting AWT can take
     * a moment and must never hold up the window.
     */
    void start() {
        if (started) {
            return;
        }
        started = true;
        Thread setup = new Thread(this::install, "dedie-tray");
        setup.setDaemon(true);
        setup.start();
    }

    private void install() {
        try {
            // Without this macOS gives AWT a second icon in the Dock.
            if (System.getProperty("apple.awt.UIElement") == null) {
                System.setProperty("apple.awt.UIElement", "true");
            }
            if (GraphicsEnvironment.isHeadless() || !SystemTray.isSupported()) {
                return;
            }
            SystemTray tray = SystemTray.getSystemTray();
            TrayIcon created = new TrayIcon(image(tray.getTrayIconSize()), "Dedie");
            created.setImageAutoSize(true);
            // Fired by a click on a notification, and by activating the icon.
            created.addActionListener(event -> open());
            created.addMouseListener(new MouseAdapter() {
                @Override
                public void mouseClicked(MouseEvent event) {
                    if (event.getButton() == MouseEvent.BUTTON1) {
                        open();
                    }
                }
            });
            tray.add(created);
            icon = created;
            if (closed) {
                close();
            }
        } catch (Exception | LinkageError | java.awt.AWTError e) {
            // No usable tray on this system. LinkageError covers a Java
            // runtime shipped without its desktop libraries.
            icon = null;
        }
    }

    /** Whether a notification passed to {@link #show} would appear. */
    boolean available() {
        return (icon != null || notifySend != null) && !closed;
    }

    /** Looks for notify-send on the PATH of a Linux session with a display. */
    private static String findNotifySend() {
        if (!System.getProperty("os.name", "").toLowerCase().contains("linux") || GraphicsEnvironment.isHeadless()) {
            return null;
        }
        for (String dir : System.getenv().getOrDefault("PATH", "").split(java.io.File.pathSeparator)) {
            java.io.File candidate = new java.io.File(dir, "notify-send");
            if (candidate.canExecute()) {
                return candidate.getPath();
            }
        }
        return null;
    }

    /** Hands a notification to the desktop's notification service. */
    private boolean sendToDesktop(String title, String body) {
        try {
            // The arguments are passed as they are, with no shell between.
            new ProcessBuilder(notifySend, "--app-name=Dedie", "--icon=dialog-information", "--", title, body)
                    .redirectErrorStream(true).redirectOutput(ProcessBuilder.Redirect.DISCARD).start();
            return true;
        } catch (java.io.IOException | RuntimeException e) {
            return false;
        }
    }

    /**
     * Shows a notification by the tray icon. Clicking it, or the icon,
     * opens the page named by link.
     */
    void show(String title, String body, String link) {
        if (closed) {
            return;
        }
        // A click on the tray icon opens the page, whichever way the
        // notification itself was shown.
        pendingLink = link;
        if (notifySend != null && sendToDesktop(title, body)) {
            return;
        }
        TrayIcon target = icon;
        if (target == null) {
            return;
        }
        EventQueue.invokeLater(() -> {
            try {
                target.displayMessage(title, body, TrayIcon.MessageType.INFO);
            } catch (RuntimeException e) {
                // The tray went away, as it does when a desktop shell restarts.
            }
        });
    }

    private void open() {
        String link = pendingLink;
        pendingLink = null;
        onOpen.accept(link);
    }

    /** Takes the icon out of the tray, which also lets AWT shut down. */
    void close() {
        closed = true;
        TrayIcon target = icon;
        if (target == null) {
            return;
        }
        icon = null;
        EventQueue.invokeLater(() -> {
            try {
                SystemTray.getSystemTray().remove(target);
            } catch (RuntimeException e) {
                // Nothing to remove it from.
            }
        });
    }

    /** Draws the icon: the initial on the colour the interface uses for its accents. */
    private static Image image(Dimension size) {
        int side = Math.max(16, Math.max(size.width, size.height));
        BufferedImage image = new BufferedImage(side, side, BufferedImage.TYPE_INT_ARGB);
        Graphics2D g = image.createGraphics();
        try {
            g.setRenderingHint(RenderingHints.KEY_ANTIALIASING, RenderingHints.VALUE_ANTIALIAS_ON);
            g.setRenderingHint(RenderingHints.KEY_TEXT_ANTIALIASING, RenderingHints.VALUE_TEXT_ANTIALIAS_ON);
            g.setColor(new Color(0x1f, 0x5f, 0xd1));
            int arc = Math.max(4, side / 4);
            g.fillRoundRect(0, 0, side, side, arc, arc);
            g.setColor(Color.WHITE);
            g.setFont(new Font(Font.SANS_SERIF, Font.BOLD, Math.round(side * 0.7f)));
            FontMetrics metrics = g.getFontMetrics();
            int x = (side - metrics.stringWidth("D")) / 2;
            int y = (side - metrics.getHeight()) / 2 + metrics.getAscent();
            g.drawString("D", x, y);
        } finally {
            g.dispose();
        }
        return image;
    }
}
