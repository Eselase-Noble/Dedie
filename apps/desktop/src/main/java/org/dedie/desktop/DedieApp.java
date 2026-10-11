package org.dedie.desktop;

import java.nio.file.Path;

import javafx.application.Application;
import javafx.application.Platform;
import javafx.concurrent.Worker;
import javafx.geometry.Insets;
import javafx.geometry.Pos;
import javafx.scene.Scene;
import javafx.scene.control.Alert;
import javafx.scene.control.Button;
import javafx.scene.control.ButtonType;
import javafx.scene.control.Label;
import javafx.scene.control.ProgressIndicator;
import javafx.scene.layout.StackPane;
import javafx.scene.layout.VBox;
import javafx.scene.web.WebEngine;
import javafx.scene.web.WebView;
import javafx.stage.Stage;

/**
 * The application window: a status screen while the engine starts, then the
 * Dedie interface.
 */
public final class DedieApp extends Application {

    private final Engine engine = new Engine();
    private final StackPane root = new StackPane();
    private Stage stage;

    @Override
    public void start(Stage primaryStage) {
        stage = primaryStage;
        root.setStyle("-fx-background-color: white;");
        showStatus("Starting Dedie…", true);

        stage.setTitle("Dedie");
        stage.setScene(new Scene(root, 1360, 860));
        stage.setMinWidth(900);
        stage.setMinHeight(600);
        stage.show();

        // The engine is started off the interface thread; the window stays
        // responsive while it opens its database and binds a port.
        engine.start().whenComplete((url, error) -> Platform.runLater(() -> {
            if (error != null) {
                showFailure(error.getMessage());
            } else {
                showInterface(url);
            }
        }));
    }

    @Override
    public void stop() {
        engine.stop();
    }

    private void showStatus(String message, boolean busy) {
        Label label = new Label(message);
        label.setStyle("-fx-font-size: 15px; -fx-text-fill: #334155;");
        label.setWrapText(true);
        label.setMaxWidth(560);
        VBox box = new VBox(18);
        box.setAlignment(Pos.CENTER);
        box.setPadding(new Insets(40));
        if (busy) {
            box.getChildren().add(new ProgressIndicator());
        }
        box.getChildren().add(label);
        root.getChildren().setAll(box);
    }

    private void showFailure(String message) {
        showStatus(message, false);
        Button quit = new Button("Quit");
        quit.setOnAction(e -> Platform.exit());
        ((VBox) root.getChildren().get(0)).getChildren().add(quit);
    }

    private void showInterface(String url) {
        WebView view = new WebView();
        view.setContextMenuEnabled(false);
        WebEngine web = view.getEngine();

        // Keeps the browser-side preferences (last connection, query
        // history) between runs, alongside the rest of Dedie's data.
        web.setUserDataDirectory(Path.of(System.getProperty("user.home"), ".dedie", "desktop").toFile());

        // The window only ever shows the local engine. Anything else, such
        // as a link in a result, opens in the system browser instead.
        web.locationProperty().addListener((observable, previous, location) -> {
            if (location != null && !location.startsWith(url) && !location.startsWith("about:")) {
                Platform.runLater(() -> web.load(previous != null ? previous : url));
                getHostServices().showDocument(location);
            }
        });
        web.setCreatePopupHandler(features -> null);

        web.setConfirmHandler(message -> {
            Alert alert = new Alert(Alert.AlertType.CONFIRMATION, message, ButtonType.OK, ButtonType.CANCEL);
            alert.initOwner(stage);
            alert.setHeaderText(null);
            return alert.showAndWait().filter(ButtonType.OK::equals).isPresent();
        });
        web.setOnAlert(event -> {
            Alert alert = new Alert(Alert.AlertType.INFORMATION, event.getData(), ButtonType.OK);
            alert.initOwner(stage);
            alert.setHeaderText(null);
            alert.showAndWait();
        });

        web.titleProperty().addListener((observable, previous, title) ->
                stage.setTitle(title == null || title.isBlank() ? "Dedie" : title));

        web.getLoadWorker().stateProperty().addListener((observable, previous, state) -> {
            if (state == Worker.State.FAILED) {
                showFailure("The interface could not be loaded from " + url + ".");
            } else if (state == Worker.State.SUCCEEDED && Boolean.getBoolean("dedie.smoke")) {
                smokeTest(web);
            }
        });

        root.getChildren().setAll(view);
        web.load(url);
    }

    /**
     * With {@code -Ddedie.smoke=true} the application waits for the page to
     * render, prints what it sees and exits. Used to check a build on a
     * machine without anyone watching the window.
     */
    private void smokeTest(WebEngine web) {
        Thread wait = new Thread(() -> {
            try {
                Thread.sleep(4000);
            } catch (InterruptedException ignored) {
                return;
            }
            Platform.runLater(() -> {
                Object text = web.executeScript("document.title + ' | ' + location.pathname + ' | ' + "
                        + "document.body.innerText.replace(/\\s+/g, ' ').slice(0, 160)");
                System.out.println("SMOKE " + text);
                Platform.exit();
            });
        }, "dedie-smoke-test");
        wait.setDaemon(true);
        wait.start();
    }
}
