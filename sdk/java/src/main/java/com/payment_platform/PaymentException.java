package com.payment_platform;

public class PaymentException extends Exception {
    public final int statusCode;
    public final String errorCode;

    public PaymentException(String message, int statusCode, String errorCode) {
        super(message);
        this.statusCode = statusCode;
        this.errorCode = errorCode;
    }
}
