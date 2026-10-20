#include "qt_gui_bridge.h"

#include <QApplication>
#include <QHBoxLayout>
#include <QLabel>
#include <QPushButton>
#include <QTimer>
#include <QVBoxLayout>
#include <QWidget>

#include <cstring>

namespace {

QString frequencyText(uint32_t frequency) {
    if (frequency == 0) {
        return QString::fromUtf8("—");
    }
    return QString::number(static_cast<double>(frequency) / 1000000.0, 'f', 6) + QStringLiteral(" MHz");
}

void updateLED(QLabel *led, bool connected) {
    const char *color = connected ? "#20a45b" : "#c0392b";
    led->setStyleSheet(QStringLiteral("QLabel { background-color: %1; border: 1px solid #555; border-radius: 9px; }").arg(QString::fromLatin1(color)));
}

QLabel *addStatusRow(QVBoxLayout *layout, const QString &label) {
    auto *row = new QWidget;
    auto *rowLayout = new QHBoxLayout(row);
    rowLayout->setContentsMargins(0, 2, 0, 2);
    auto *led = new QLabel(row);
    led->setFixedSize(18, 18);
    updateLED(led, false);
    rowLayout->addWidget(led);
    auto *text = new QLabel(label, row);
    text->setStyleSheet(QStringLiteral("font-size: 14px;"));
    rowLayout->addWidget(text);
    rowLayout->addStretch(1);
    layout->addWidget(row);
    return led;
}

} // namespace

extern "C" void qt_bridge_run(void) {
    int argc = 1;
    char appName[] = "gpredict_vrn76_bridge";
    char *argv[] = {appName, nullptr};
    QApplication app(argc, argv);

    QWidget window;
    window.setWindowTitle(QStringLiteral("Gpredict / VR-N76 Bridge"));
    window.resize(430, 260);

    auto *layout = new QVBoxLayout(&window);
    layout->setContentsMargins(24, 20, 24, 20);
    layout->setSpacing(12);

    auto *title = new QLabel(QStringLiteral("Satellite radio link"), &window);
    title->setStyleSheet(QStringLiteral("font-size: 18px; font-weight: 600;"));
    layout->addWidget(title);

    QLabel *gpredictLED = addStatusRow(layout, QStringLiteral("Gpredict / rigctld"));
    QLabel *radioLED = addStatusRow(layout, QStringLiteral("VR-N76 / RFCOMM"));

    auto *rxFrequency = new QLabel(&window);
    auto *txFrequency = new QLabel(&window);
    rxFrequency->setStyleSheet(QStringLiteral("font-size: 17px; font-weight: 600;"));
    txFrequency->setStyleSheet(QStringLiteral("font-size: 17px; font-weight: 600;"));
    layout->addWidget(rxFrequency);
    layout->addWidget(txFrequency);

    auto *connectButton = new QPushButton(QStringLiteral("Connect VR-N76"), &window);
    auto *buttonLayout = new QHBoxLayout;
    auto *disconnectButton = new QPushButton(QStringLiteral("Disconnect VR-N76"), &window);
    auto *exitButton = new QPushButton(QStringLiteral("Exit"), &window);
    buttonLayout->addWidget(connectButton);
    buttonLayout->addWidget(disconnectButton);
    buttonLayout->addWidget(exitButton);
    layout->addLayout(buttonLayout);
    auto *errorLabel = new QLabel(&window);
    errorLabel->setWordWrap(true);
    errorLabel->setStyleSheet(QStringLiteral("color: #a93226;"));
    layout->addWidget(errorLabel);

    QObject::connect(connectButton, &QPushButton::clicked, &window, []() {
        goConnectVRN76();
    });
    QObject::connect(disconnectButton, &QPushButton::clicked, &window, []() {
        goDisconnectVRN76();
    });
    QObject::connect(exitButton, &QPushButton::clicked, &window, []() {
        QApplication::quit();
    });

    auto *timer = new QTimer(&window);
    QObject::connect(timer, &QTimer::timeout, &window, [=]() {
        int gpredictConnected = 0;
        int radioConnected = 0;
        int connecting = 0;
        uint32_t rxHz = 0;
        uint32_t txHz = 0;
        char errorBuffer[512] = {};
        goBridgeSnapshot(&gpredictConnected, &radioConnected, &connecting,
                         &rxHz, &txHz, errorBuffer, sizeof(errorBuffer));

        updateLED(gpredictLED, gpredictConnected != 0);
        updateLED(radioLED, radioConnected != 0);
        rxFrequency->setText(QStringLiteral("RX: ") + frequencyText(rxHz));
        txFrequency->setText(QStringLiteral("TX: ") + frequencyText(txHz));
        connectButton->setEnabled(connecting == 0 && radioConnected == 0);
        disconnectButton->setEnabled(connecting == 0 && radioConnected != 0);
        connectButton->setText(connecting != 0 ? QStringLiteral("Connecting to VR-N76…")
                                               : QStringLiteral("Connect VR-N76"));
        errorLabel->setText(QString::fromUtf8(errorBuffer));
    });
    timer->start(250);

    window.show();
    app.exec();
}
